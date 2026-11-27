// Vault reorganisation: preview what arr-style naming would do, then apply it
// as one all-or-nothing operation.
//
// The invariant everything else depends on is that every vault file still ends
// in ".<iso2>.mka": internal/naming owns the stem, [naming.VaultPath] is the
// only place the suffix is appended, and no route here can write a file name
// any other way.
//
// The apply protocol is filesystem two-phase with SQL compensation:
//
//	preflight  rebuild the plan, hash it, check jobs, sources, destinations,
//	           containment, cycles and devices — no mutation at all
//	phase A    mkdir -p the destination parents, remembering what was created
//	phase B    os.Rename every file, in plan order
//	phase C    ONE transaction: guarded UPDATEs plus one audit row per file
//	           and the run row
//
// A file moves only when the whole plan preflights clean. A partial move is a
// bug, not a fallback: a phase B failure renames everything back and returns
// 500 apply_failed; a phase C failure renames everything back too. Only when a
// rename-back itself fails does the database get reconciled to whatever is
// actually on disk, the run is marked needs_attention, and the response lists
// the paths to inspect (code manual_attention, logged at error level).
//
// Nothing here deletes media: the only os.Remove calls are for directories this
// request created and only while they are empty. No re-encode, no re-hash, no
// content rewrite — paths only.
package vaultapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Subject412/dubarr/backend/internal/naming"
	"github.com/Subject412/dubarr/backend/internal/settings"
	"github.com/Subject412/dubarr/backend/internal/store"
)

const (
	// PlanMaxFiles bounds one plan. A plan is bounded by the vault's track
	// count and the UI renders all of it; 50k rows is ~10 MB of JSON, so a
	// bigger vault has to be scoped by media type first.
	PlanMaxFiles = 20000

	// planTTL is how long a preview's plan_id can be applied. Apply rebuilds
	// the plan and refuses with stale_plan when anything moved on.
	planTTL = 10 * time.Minute
)

// Refusal codes. The UI translates these; the text is for the log and for a
// non-UI client.
const (
	codeInvalidRequest  = "invalid_request"
	codeInvalidTemplate = "invalid_template"
	codeNameInvalid     = "name_invalid"
	codePlanTooLarge    = "plan_too_large"
	codeJobsRunning     = "jobs_running"
	codeConflict        = "conflict"
	codeStalePlan       = "stale_plan"
	codePathCycle       = "path_cycle"
	codePathEscape      = "path_escape"
	codeCrossDevice     = "cross_device"
	codeVaultNotSetUp   = "vault_not_configured"
	codeApplyFailed     = "apply_failed"
	codeManualAttention = "manual_attention"
	codeInternal        = "internal"
	codeServiceUnavail  = "unavailable"
	conflictSourceGone  = "source_missing"
	conflictDestExists  = "destination_exists"
	conflictDestDup     = "destination_duplicate"
	conflictDestParent  = "destination_parent"
)

// organiseScope selects which archived tracks a plan covers.
type organiseScope struct {
	MediaType string  `json:"media_type"`
	IDs       []int64 `json:"ids"`
}

type previewRequest struct {
	Scope organiseScope `json:"scope"`
}

type applyRequest struct {
	PlanID  string `json:"plan_id"`
	Confirm bool   `json:"confirm"`
}

// PlanConflict is one name a plan cannot produce, with every track involved.
type PlanConflict struct {
	Kind        string  `json:"kind"`
	Destination string  `json:"destination"`
	TrackIDs    []int64 `json:"track_ids"`
}

// PlanFile is one row of the preview table: exactly what the UI renders.
type PlanFile struct {
	TrackID     int64   `json:"track_id"`
	MediaID     int64   `json:"media_id"`
	Source      string  `json:"source"`
	Destination string  `json:"destination"`
	Changed     bool    `json:"changed"`
	Reason      string  `json:"reason"`
	Conflict    *string `json:"conflict"`
}

// PlanImpact is the operator-facing summary of what an apply would touch.
type PlanImpact struct {
	Renamed               int `json:"renamed"`
	Unchanged             int `json:"unchanged"`
	JobsReferencingTracks int `json:"jobs_referencing_tracks"`
	// JobsBlocked is non-zero exactly when apply would be refused for a job in
	// analyzing/muxing. A held/queued job references tracks but cannot be
	// mid-mux, so it never blocks (#48).
	JobsBlocked int `json:"jobs_blocked"`
	// LibraryRowsAffected is 0 by construction: arr_library_cache points at
	// target videos, never at vault audio.
	LibraryRowsAffected int `json:"library_rows_affected"`
}

// PlanResponse is the preview payload. plan_id is absent when the plan carries
// any conflict: apply takes a plan_id, so a conflicted plan cannot be applied.
type PlanResponse struct {
	PlanID    string         `json:"plan_id,omitempty"`
	ExpiresAt string         `json:"expires_at,omitempty"`
	Files     []PlanFile     `json:"files"`
	Conflicts []PlanConflict `json:"conflicts"`
	Impact    PlanImpact     `json:"impact"`
}

// RunResponse is the result of a completed apply.
type RunResponse struct {
	RunID     int64  `json:"run_id"`
	Status    string `json:"status"`
	FileCount int    `json:"file_count"`
	Renamed   int    `json:"renamed"`
	Unchanged int    `json:"unchanged"`
}

// RunRow is one audit run as the history table renders it.
type RunRow struct {
	ID         int64  `json:"id"`
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at,omitempty"`
	Status     string `json:"status"`
	PlanHash   string `json:"plan_hash"`
	FileCount  int    `json:"file_count"`
	Actor      string `json:"actor,omitempty"`
	Detail     string `json:"detail,omitempty"`
}

type runsResponse struct {
	Items []RunRow `json:"items"`
}

// organiseError is the refusal body. Kind carries the conflict's machine name
// (source_missing, destination_exists, ...) so the UI never parses the text.
type organiseError struct {
	Code    string   `json:"code"`
	Kind    string   `json:"kind,omitempty"`
	Stage   string   `json:"stage,omitempty"`
	Undone  bool     `json:"undone,omitempty"`
	Files   []string `json:"files,omitempty"`
	Count   int      `json:"count,omitempty"`
	Message string   `json:"error"`
}

// organiseFailure is a refusal that already knows its HTTP status and code.
type organiseFailure struct {
	status  int
	code    string
	kind    string
	stage   string
	undone  bool
	files   []string
	count   int
	message string
}

func (f *organiseFailure) Error() string { return f.message }

func (f *organiseFailure) write(w http.ResponseWriter) {
	writeJSON(w, f.status, organiseError{
		Code:    f.code,
		Kind:    f.kind,
		Stage:   f.stage,
		Undone:  f.undone,
		Files:   f.files,
		Count:   f.count,
		Message: f.message,
	})
}

func internalFailure(err error) *organiseFailure {
	slog.Error("vault organise: internal failure", "error", err)
	return &organiseFailure{
		status:  http.StatusInternalServerError,
		code:    codeInternal,
		message: "the vault reorganisation could not be planned",
	}
}

// planFile is the internal, already-resolved form of one row.
type planFile struct {
	trackID     int64
	mediaID     int64
	source      string
	destination string
	changed     bool
	size        int64
	mtimeNS     int64
}

// organisePlan is a previewed plan. It is the only state apply consumes, and it
// is deliberately disposable: the database and the filesystem are the record.
type organisePlan struct {
	hash    string
	scope   organiseScope
	cfg     namingConfig
	files   []planFile
	moves   []move
	impact  PlanImpact
	expires time.Time
}

type move struct {
	trackID     int64
	source      string
	destination string
}

// namingConfig is the parsed form of vault.naming plus the vault root.
type namingConfig struct {
	movie     naming.Template
	episode   naming.Template
	season    naming.Template
	vaultDir  string
	vaultRoot string
}

// planCache holds previewed plans for planTTL. It is a cache, not state:
// restarting the process loses it and loses nothing else.
type planCache struct {
	mu      sync.Mutex
	entries map[string]*organisePlan
}

func newPlanCache() *planCache {
	return &planCache{entries: make(map[string]*organisePlan)}
}

func (c *planCache) put(p *organisePlan) {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	// Swept on write: a preview is rare enough that the map never grows past
	// the number of plans a busy operator can produce in ten minutes.
	for key, entry := range c.entries {
		if now.After(entry.expires) {
			delete(c.entries, key)
		}
	}
	c.entries[p.hash] = p
}

func (c *planCache) get(id string) (*organisePlan, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[id]
	if !ok {
		return nil, false
	}
	if time.Now().After(entry.expires) {
		delete(c.entries, id)
		return nil, false
	}
	return entry, true
}

func (c *planCache) drop(id string) {
	c.mu.Lock()
	delete(c.entries, id)
	c.mu.Unlock()
}

// handleOrganisePreview plans a rename for the scope and returns every row.
// It writes nothing: no directories, no files, no rows.
func (s *Service) handleOrganisePreview(w http.ResponseWriter, r *http.Request) {
	if s.store == nil || s.store.DB == nil {
		writeError(w, http.StatusServiceUnavailable, "vault store is not configured")
		return
	}
	req, ferr := decodePreviewRequest(r)
	if ferr != nil {
		ferr.write(w)
		return
	}
	plan, ferr := s.buildPlan(r.Context(), req.Scope)
	if ferr != nil {
		ferr.write(w)
		return
	}

	resp := PlanResponse{
		Files:     make([]PlanFile, 0, len(plan.files)),
		Conflicts: plan.conflicts(),
		Impact:    plan.impact,
	}
	if resp.Conflicts == nil {
		resp.Conflicts = []PlanConflict{}
	}
	byTrack := make(map[int64]string, len(resp.Conflicts))
	for _, c := range resp.Conflicts {
		for _, id := range c.TrackIDs {
			if _, seen := byTrack[id]; !seen {
				byTrack[id] = c.Kind
			}
		}
	}
	for _, f := range plan.files {
		row := PlanFile{
			TrackID:     f.trackID,
			MediaID:     f.mediaID,
			Source:      f.source,
			Destination: f.destination,
			Changed:     f.changed,
			Reason:      "template_change",
		}
		if !f.changed {
			row.Reason = "unchanged"
		}
		if kind, ok := byTrack[f.trackID]; ok {
			k := kind
			row.Conflict = &k
		}
		resp.Files = append(resp.Files, row)
	}

	if len(resp.Conflicts) == 0 {
		s.plans.put(plan)
		resp.PlanID = plan.hash
		resp.ExpiresAt = plan.expires.UTC().Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleOrganiseApply re-plans, re-checks everything, and only then moves files.
func (s *Service) handleOrganiseApply(w http.ResponseWriter, r *http.Request) {
	if s.store == nil || s.store.DB == nil {
		writeError(w, http.StatusServiceUnavailable, "vault store is not configured")
		return
	}
	req, ferr := decodeApplyRequest(r)
	if ferr != nil {
		ferr.write(w)
		return
	}
	if !req.Confirm {
		(&organiseFailure{
			status:  http.StatusBadRequest,
			code:    codeInvalidRequest,
			message: "confirm must be true: applying a rename plan is an explicit operator action",
		}).write(w)
		return
	}

	// One apply at a time. A second concurrent apply waits here and then finds
	// its plan gone (or its hash stale), which is the documented behaviour.
	s.applyMu.Lock()
	defer s.applyMu.Unlock()

	cached, ok := s.plans.get(req.PlanID)
	if !ok {
		writeStalePlan(w)
		return
	}

	plan, ferr := s.buildPlan(r.Context(), cached.scope)
	if ferr != nil {
		ferr.write(w)
		return
	}
	// The TOCTOU guard: the same tuples (track, source, destination, size,
	// mtime) must still hash to the plan the operator was shown.
	if plan.hash != req.PlanID {
		writeStalePlan(w)
		return
	}
	if conflicts := plan.conflicts(); len(conflicts) > 0 {
		(&organiseFailure{
			status:  http.StatusConflict,
			code:    codeConflict,
			kind:    conflicts[0].Kind,
			message: conflictMessage(conflicts[0]),
			files:   []string{conflicts[0].Destination},
		}).write(w)
		return
	}
	if plan.impact.JobsBlocked > 0 {
		(&organiseFailure{
			status:  http.StatusConflict,
			code:    codeJobsRunning,
			count:   plan.impact.JobsBlocked,
			message: "a sync job is analysing or muxing right now; nothing was changed",
		}).write(w)
		return
	}
	if ferr := preflightApply(plan); ferr != nil {
		ferr.write(w)
		return
	}

	resp, ferr := s.runApply(r.Context(), plan)
	if ferr != nil {
		ferr.write(w)
		return
	}
	// A successfully applied plan is spent: its hash no longer describes the
	// vault, and the cache is not state.
	s.plans.drop(req.PlanID)
	writeJSON(w, http.StatusOK, resp)
}

// handleOrganiseRuns serves the audit history, newest first. It reads only the
// database, so a restart (which empties the plan cache) loses nothing.
func (s *Service) handleOrganiseRuns(w http.ResponseWriter, r *http.Request) {
	if s.store == nil || s.store.DB == nil {
		writeError(w, http.StatusServiceUnavailable, "vault store is not configured")
		return
	}
	limit := 50
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 500 {
			(&organiseFailure{
				status:  http.StatusBadRequest,
				code:    codeInvalidRequest,
				message: "limit must be 1..500",
			}).write(w)
			return
		}
		limit = n
	}
	rows, err := s.store.ListOrganiseRuns(r.Context(), limit)
	if err != nil {
		internalFailure(err).write(w)
		return
	}
	items := make([]RunRow, 0, len(rows))
	for _, row := range rows {
		items = append(items, RunRow{
			ID:         row.ID,
			StartedAt:  row.StartedAt,
			FinishedAt: row.FinishedAt,
			Status:     row.Status,
			PlanHash:   row.PlanHash,
			FileCount:  row.FileCount,
			Actor:      row.Actor,
			Detail:     row.Detail,
		})
	}
	writeJSON(w, http.StatusOK, runsResponse{Items: items})
}

func writeStalePlan(w http.ResponseWriter) {
	(&organiseFailure{
		status:  http.StatusConflict,
		code:    codeStalePlan,
		message: "the vault changed since the preview; preview again",
	}).write(w)
}

func conflictMessage(c PlanConflict) string {
	switch c.Kind {
	case conflictDestDup:
		return "two files would get the same name; nothing was changed"
	case conflictDestExists:
		return "a name is already taken; nothing was changed"
	case conflictSourceGone:
		return "a track's file is missing from the vault; nothing was changed"
	}
	return "the plan cannot be applied; nothing was changed"
}

// buildPlan renders every in-scope track and checks the filesystem invariants
// that can be checked without mutating anything. It returns a failure (never a
// partially built plan) for a template, scope or size problem.
func (s *Service) buildPlan(ctx context.Context, scope organiseScope) (*organisePlan, *organiseFailure) {
	cfg, ferr := s.namingConfig(ctx)
	if ferr != nil {
		return nil, ferr
	}
	if scope.MediaType == "" {
		scope.MediaType = "all"
	}
	switch scope.MediaType {
	case "all", "movie", "series":
	default:
		return nil, &organiseFailure{
			status:  http.StatusBadRequest,
			code:    codeInvalidRequest,
			message: fmt.Sprintf("scope.media_type must be movie, series or all, not %q", scope.MediaType),
		}
	}

	rows, err := s.store.VaultTrackPlanRows(ctx, scope.MediaType, scope.IDs)
	if err != nil {
		return nil, internalFailure(err)
	}
	if len(rows) > PlanMaxFiles {
		return nil, &organiseFailure{
			status: http.StatusConflict,
			code:   codePlanTooLarge,
			count:  len(rows),
			message: fmt.Sprintf(
				"%d files is above the %d-file plan limit; scope the preview by media type",
				len(rows), PlanMaxFiles),
		}
	}

	plan := &organisePlan{
		scope:   scope,
		cfg:     cfg,
		files:   make([]planFile, 0, len(rows)),
		expires: time.Now().Add(planTTL),
	}
	sources := make(map[string]int64, len(rows))
	for _, row := range rows {
		sources[filepath.Clean(row.FilePath)] = row.TrackID
	}

	for _, row := range rows {
		destination, rerr := renderDestination(cfg, row)
		if rerr != nil {
			return nil, &organiseFailure{
				status:  http.StatusConflict,
				code:    codeNameInvalid,
				message: fmt.Sprintf("track %d (%s): %v", row.TrackID, filepath.Base(row.FilePath), rerr),
				files:   []string{row.FilePath},
			}
		}
		plan.files = append(plan.files, planFile{
			trackID:     row.TrackID,
			mediaID:     row.MediaID,
			source:      filepath.Clean(row.FilePath),
			destination: filepath.Clean(destination),
			changed:     filepath.Clean(destination) != filepath.Clean(row.FilePath),
		})
	}

	for i := range plan.files {
		f := &plan.files[i]
		info, statErr := os.Lstat(f.source)
		if statErr != nil || !info.Mode().IsRegular() {
			// A symlink or a directory is not an archived track, even if the
			// row says it is.
			continue
		}
		f.size = info.Size()
		f.mtimeNS = info.ModTime().UnixNano()
	}

	impact := PlanImpact{}
	ids := make([]int64, 0, len(plan.files))
	for _, f := range plan.files {
		ids = append(ids, f.trackID)
		if f.changed {
			impact.Renamed++
		} else {
			impact.Unchanged++
		}
	}
	counts, err := s.store.JobsInFlight(ctx, ids)
	if err != nil {
		return nil, internalFailure(err)
	}
	impact.JobsReferencingTracks = counts.Referencing
	impact.JobsBlocked = counts.Blocking
	plan.impact = impact

	plan.hash = hashPlan(plan.files)
	for _, f := range plan.files {
		if f.changed {
			plan.moves = append(plan.moves, move{trackID: f.trackID, source: f.source, destination: f.destination})
		}
	}
	return plan, nil
}

// conflicts re-derives the plan's conflicts from the filesystem. It is called
// twice on purpose (preview renders them, apply refuses on them) so a file that
// appeared between the two cannot slip through.
func (p *organisePlan) conflicts() []PlanConflict {
	sources := make(map[string]int64, len(p.files))
	for _, f := range p.files {
		sources[f.source] = f.trackID
	}

	byDestination := make(map[string][]int64)
	for _, f := range p.files {
		if !f.changed {
			continue
		}
		byDestination[f.destination] = append(byDestination[f.destination], f.trackID)
	}

	var out []PlanConflict
	for destination, ids := range byDestination {
		if len(ids) > 1 {
			sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
			out = append(out, PlanConflict{Kind: conflictDestDup, Destination: destination, TrackIDs: ids})
		}
	}
	for _, f := range p.files {
		info, err := os.Lstat(f.source)
		if err != nil || !info.Mode().IsRegular() {
			out = append(out, PlanConflict{Kind: conflictSourceGone, Destination: f.source, TrackIDs: []int64{f.trackID}})
			continue
		}
		if !f.changed {
			continue
		}
		// A destination that is another planned track's source is a swap: the
		// only safe answer is path_cycle at apply, not "the name is taken".
		if owner, ok := sources[f.destination]; ok && owner != f.trackID {
			continue
		}
		if _, err := os.Lstat(f.destination); err == nil {
			out = append(out, PlanConflict{Kind: conflictDestExists, Destination: f.destination, TrackIDs: []int64{f.trackID}})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Destination < out[j].Destination
	})
	return out
}

// renderDestination turns one plan row into its destination path. The media
// folder is taken from vault_media (Dubarr already owns it); the only directory
// a template may add is the season subfolder.
func renderDestination(cfg namingConfig, row store.VaultTrackPlanRow) (string, error) {
	tr := naming.Track{
		Title:        row.Title,
		Year:         row.Year,
		Variant:      row.Variant,
		Codec:        row.Codec,
		Channels:     row.Channels,
		LangISO2:     row.LangISO2,
		EpisodeTitle: row.EpisodeTitle,
	}
	if row.Season != nil {
		tr.Season = *row.Season
	}
	if row.Episode != nil {
		tr.Episode = *row.Episode
	}

	seasonFolder := ""
	if row.Season != nil && *row.Season > 0 {
		folder, err := cfg.season.RenderFolder(tr)
		if err != nil {
			return "", err
		}
		seasonFolder = folder
	}

	tmpl := cfg.movie
	if row.MediaType == "series" {
		tmpl = cfg.episode
	}
	stem, err := tmpl.Render(tr)
	if err != nil {
		return "", err
	}
	destination := naming.VaultPath(row.MediaFolderPath, seasonFolder, tr, stem)
	if len(destination) > naming.MaxPathBytes {
		return "", fmt.Errorf("destination path is %d bytes; the limit is %d", len(destination), naming.MaxPathBytes)
	}
	return destination, nil
}

// hashPlan is sha256 over the sorted tuple list (track_id, source, destination,
// size, mtime_ns) — the TOCTOU guard apply re-derives.
func hashPlan(files []planFile) string {
	lines := make([]string, 0, len(files))
	for _, f := range files {
		lines = append(lines, fmt.Sprintf("%d\x00%s\x00%s\x00%d\x00%d",
			f.trackID, f.source, f.destination, f.size, f.mtimeNS))
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// namingConfig reads the templates and the vault root. A template that no
// longer validates is refused here rather than half-applied: settings refuses
// such a write, but a hand-edited row is still possible.
func (s *Service) namingConfig(ctx context.Context) (namingConfig, *organiseFailure) {
	var cfg namingConfig
	if s.settings == nil {
		return cfg, &organiseFailure{
			status:  http.StatusServiceUnavailable,
			code:    codeServiceUnavail,
			message: "settings are not configured",
		}
	}

	section := settings.VaultNaming{
		MovieTemplate:   settings.DefaultMovieTemplate,
		EpisodeTemplate: settings.DefaultEpisodeTemplate,
		SeasonFolder:    settings.DefaultSeasonFolder,
	}
	if raw, ok, err := s.settings.Get(ctx, settings.KeyVaultNaming); err != nil {
		return cfg, internalFailure(err)
	} else if ok {
		if err := json.Unmarshal(raw, &section); err != nil {
			return cfg, &organiseFailure{
				status:  http.StatusConflict,
				code:    codeInvalidTemplate,
				message: fmt.Sprintf("%s is not readable: %v", settings.KeyVaultNaming, err),
			}
		}
	}

	for _, field := range []struct {
		name string
		raw  string
		dst  *naming.Template
	}{
		{"movie_template", section.MovieTemplate, &cfg.movie},
		{"episode_template", section.EpisodeTemplate, &cfg.episode},
		{"season_folder", section.SeasonFolder, &cfg.season},
	} {
		if field.name != "season_folder" && strings.TrimSpace(field.raw) == "" {
			return cfg, &organiseFailure{
				status:  http.StatusConflict,
				code:    codeInvalidTemplate,
				message: fmt.Sprintf("%s.%s is empty", settings.KeyVaultNaming, field.name),
			}
		}
		tmpl, err := naming.Parse(field.raw)
		if err != nil {
			return cfg, &organiseFailure{
				status:  http.StatusConflict,
				code:    codeInvalidTemplate,
				message: fmt.Sprintf("%s.%s: %v", settings.KeyVaultNaming, field.name, err),
			}
		}
		*field.dst = tmpl
	}

	var paths settings.StoragePaths
	if raw, ok, err := s.settings.Get(ctx, settings.KeyStoragePaths); err != nil {
		return cfg, internalFailure(err)
	} else if ok {
		if err := json.Unmarshal(raw, &paths); err != nil {
			return cfg, &organiseFailure{
				status:  http.StatusConflict,
				code:    codeVaultNotSetUp,
				message: fmt.Sprintf("%s is not readable: %v", settings.KeyStoragePaths, err),
			}
		}
	}
	if strings.TrimSpace(paths.VaultDir) == "" {
		return cfg, &organiseFailure{
			status:  http.StatusConflict,
			code:    codeVaultNotSetUp,
			message: "storage.paths.vault_dir is not configured",
		}
	}
	cfg.vaultDir = paths.VaultDir
	root, err := filepath.EvalSymlinks(paths.VaultDir)
	if err != nil {
		return cfg, &organiseFailure{
			status:  http.StatusConflict,
			code:    codeVaultNotSetUp,
			message: fmt.Sprintf("the vault folder %s is not usable: %v", paths.VaultDir, err),
		}
	}
	cfg.vaultRoot = root
	return cfg, nil
}

// preflightApply re-checks every invariant that needs the filesystem, before a
// single byte moves. Any failure means nothing was changed.
func preflightApply(plan *organisePlan) *organiseFailure {
	root := plan.cfg.vaultRoot
	sources := make(map[string]int64, len(plan.files))
	for _, f := range plan.files {
		sources[f.source] = f.trackID
	}

	seenDestination := make(map[string]int64, len(plan.moves))
	for _, m := range plan.moves {
		// Containment: the destination must stay inside the vault root, after
		// both sides are resolved, so neither a symlinked VAULT_DIR nor a
		// symlinked folder inside it can redirect a file out of the vault.
		resolved, err := resolvePath(m.destination)
		if err != nil {
			return &organiseFailure{
				status:  http.StatusConflict,
				code:    codeConflict,
				kind:    conflictDestParent,
				files:   []string{m.destination},
				message: fmt.Sprintf("the destination %s cannot be resolved: %v; nothing was changed", m.destination, err),
			}
		}
		rel, err := filepath.Rel(root, resolved)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return &organiseFailure{
				status:  http.StatusConflict,
				code:    codePathEscape,
				files:   []string{m.destination},
				message: fmt.Sprintf("%s is outside %s; nothing was changed", m.destination, plan.cfg.vaultDir),
			}
		}

		sourceInfo, err := os.Lstat(m.source)
		if err != nil || !sourceInfo.Mode().IsRegular() {
			return &organiseFailure{
				status:  http.StatusConflict,
				code:    codeConflict,
				kind:    conflictSourceGone,
				files:   []string{m.source},
				message: "a track's file is missing from the vault; nothing was changed",
			}
		}

		if other, dup := seenDestination[m.destination]; dup {
			return &organiseFailure{
				status:  http.StatusConflict,
				code:    codeConflict,
				kind:    conflictDestDup,
				files:   []string{m.destination},
				message: fmt.Sprintf("tracks %d and %d would get the same name; nothing was changed", other, m.trackID),
			}
		}
		seenDestination[m.destination] = m.trackID

		// A destination that is another track's source is a swap. audio_tracks
		// .file_path is UNIQUE, so it cannot be done in one pass — refuse.
		if owner, ok := sources[m.destination]; ok && owner != m.trackID {
			return &organiseFailure{
				status:  http.StatusConflict,
				code:    codePathCycle,
				files:   []string{m.destination},
				message: "the new names would swap two files; nothing was changed — adjust the pattern",
			}
		}

		if _, err := os.Lstat(m.destination); err == nil {
			return &organiseFailure{
				status:  http.StatusConflict,
				code:    codeConflict,
				kind:    conflictDestExists,
				files:   []string{m.destination},
				message: "a name is already taken; nothing was changed",
			}
		}

		// Same device: a rename across filesystems would fail halfway (EXDEV is
		// only discoverable by trying, so the device numbers are compared).
		parent := filepath.Dir(m.destination)
		existingParent, err := nearestExistingDir(parent)
		if err != nil {
			return &organiseFailure{
				status:  http.StatusConflict,
				code:    codeConflict,
				kind:    conflictDestParent,
				files:   []string{parent},
				message: fmt.Sprintf("the destination folder %s is not usable: %v", parent, err),
			}
		}
		if !sameDevice(sourceInfo, existingParent) {
			return &organiseFailure{
				status:  http.StatusConflict,
				code:    codeCrossDevice,
				files:   []string{m.destination},
				message: "the destination is on a different filesystem; nothing was changed",
			}
		}
	}
	return nil
}

// resolvePath returns the canonical form of path, resolving every symlink in
// the deepest ancestor that exists. A plan's destination usually does not exist
// yet (phase A creates a season folder), so EvalSymlinks on the whole path is
// not enough — and a VAULT_DIR that is itself a symlink must still compare equal
// against the paths built from it.
func resolvePath(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	parent := filepath.Dir(path)
	if parent == path {
		return "", err
	}
	resolvedParent, err := resolvePath(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolvedParent, filepath.Base(path)), nil
}

// nearestExistingDir walks up from dir to the first path that exists, so the
// device and writability of a folder that has yet to be created can be checked
// before phase A.
func nearestExistingDir(dir string) (os.FileInfo, error) {
	for {
		info, err := os.Stat(dir)
		if err == nil {
			if !info.IsDir() {
				return nil, errors.New("not a directory")
			}
			return info, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil, err
		}
		dir = parent
	}
}

func sameDevice(a, b os.FileInfo) bool {
	sa, ok := a.Sys().(*syscall.Stat_t)
	if !ok {
		return true
	}
	sb, ok := b.Sys().(*syscall.Stat_t)
	if !ok {
		return true
	}
	return sa.Dev == sb.Dev
}

// runApply performs the two-phase apply and its compensation.
func (s *Service) runApply(ctx context.Context, plan *organisePlan) (RunResponse, *organiseFailure) {
	if len(plan.moves) == 0 {
		runID, err := s.recordRun(ctx, plan, nil, store.OrganiseStatusApplied, "no changes")
		if err != nil {
			return RunResponse{}, internalFailure(err)
		}
		return RunResponse{
			RunID:     runID,
			Status:    store.OrganiseStatusApplied,
			FileCount: 0,
			Unchanged: plan.impact.Unchanged,
		}, nil
	}

	created, ferr := ensureDirs(plan.moves)
	if ferr != nil {
		removeEmptyDirs(created)
		return RunResponse{}, ferr
	}

	// Phase B — rename. Every rename is undone, in reverse, on the first error.
	renamed := make([]move, 0, len(plan.moves))
	for _, m := range plan.moves {
		if err := s.renamer()(m.source, m.destination); err != nil {
			undone, ok := s.undoRenames(renamed)
			if !ok {
				return s.reconcile(ctx, plan, renamed, plan.moves[len(renamed):], fmt.Sprintf("stage=rename: %v", err))
			}
			removeEmptyDirs(created)
			detail := fmt.Sprintf("stage=rename: %v", err)
			s.recordRolledBack(ctx, plan, renamed, detail)
			return RunResponse{}, &organiseFailure{
				status:  http.StatusInternalServerError,
				code:    codeApplyFailed,
				stage:   "rename",
				undone:  undone,
				files:   []string{m.source},
				message: fmt.Sprintf("renaming %s failed and the earlier renames were undone: %v", filepath.Base(m.source), err),
			}
		}
		renamed = append(renamed, m)
	}

	// Phase C — SQL, one transaction. The WHERE file_path = source guard is what
	// makes a concurrent apply (or a changed row) abort instead of overwrite.
	runID, err := s.commitPaths(ctx, plan, renamed)
	if err != nil {
		undone, ok := s.undoRenames(renamed)
		if !ok {
			return s.reconcile(ctx, plan, renamed, nil, fmt.Sprintf("stage=sql: %v", err))
		}
		removeEmptyDirs(created)
		detail := fmt.Sprintf("stage=sql: %v", err)
		s.recordRolledBack(ctx, plan, renamed, detail)
		return RunResponse{}, &organiseFailure{
			status:  http.StatusInternalServerError,
			code:    codeApplyFailed,
			stage:   "sql",
			undone:  undone,
			message: fmt.Sprintf("recording the new paths failed and every rename was undone: %v", err),
		}
	}

	return RunResponse{
		RunID:     runID,
		Status:    store.OrganiseStatusApplied,
		FileCount: len(renamed),
		Renamed:   len(renamed),
		Unchanged: plan.impact.Unchanged,
	}, nil
}

// renamer returns the rename function (os.Rename unless a test swapped it).
func (s *Service) renamer() func(string, string) error {
	if s.renameFile != nil {
		return s.renameFile
	}
	return os.Rename
}

// ensureDirs creates every destination parent, recording only the directories
// this call created so an undo can remove exactly those.
func ensureDirs(moves []move) ([]string, *organiseFailure) {
	created := make([]string, 0, len(moves))
	for _, m := range moves {
		parent := filepath.Dir(m.destination)
		if err := mkdirAll(parent, &created); err != nil {
			return created, &organiseFailure{
				status:  http.StatusInternalServerError,
				code:    codeApplyFailed,
				stage:   "mkdir",
				undone:  true,
				files:   []string{parent},
				message: fmt.Sprintf("creating %s failed; nothing was changed: %v", parent, err),
			}
		}
	}
	return created, nil
}

// mkdirAll creates dir and any missing parents, appending every directory it
// actually created to created (outermost first).
func mkdirAll(dir string, created *[]string) error {
	var missing []string
	current := dir
	for {
		info, err := os.Stat(current)
		if err == nil {
			if !info.IsDir() {
				return fmt.Errorf("%s is not a directory", current)
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		missing = append(missing, current)
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	for i := len(missing) - 1; i >= 0; i-- {
		if err := os.Mkdir(missing[i], 0o755); err != nil {
			if errors.Is(err, os.ErrExist) {
				continue
			}
			return err
		}
		*created = append(*created, missing[i])
	}
	return nil
}

// undoRenames renames the applied moves back, newest first. ok is false when at
// least one rename could not be undone, which is the only path to reconcile.
func (s *Service) undoRenames(done []move) (undone bool, ok bool) {
	ok = true
	for i := len(done) - 1; i >= 0; i-- {
		if err := s.renamer()(done[i].destination, done[i].source); err != nil {
			slog.Error("vault organise: rename-back failed",
				"track_id", done[i].trackID,
				"from", done[i].destination,
				"to", done[i].source,
				"error", err)
			ok = false
		}
	}
	return ok, ok
}

// removeEmptyDirs removes directories this request created, deepest first, and
// only when they are empty (os.Remove refuses a non-empty directory).
func removeEmptyDirs(created []string) {
	for i := len(created) - 1; i >= 0; i-- {
		if err := os.Remove(created[i]); err != nil && !errors.Is(err, os.ErrNotExist) {
			// A non-empty directory is not an error: another file landed there.
			continue
		}
	}
}

// commitPaths is phase C: guarded path updates plus the audit rows, in one
// transaction. A zero-row UPDATE aborts it (store.ErrTrackPathChanged).
func (s *Service) commitPaths(ctx context.Context, plan *organisePlan, moves []move) (int64, error) {
	tx, err := s.store.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed

	// BEGIN IMMEDIATE equivalent: the first statement is the one that takes the
	// write lock, instead of upgrading a deferred read lock later (an upgrade
	// under a concurrent writer fails with SQLITE_BUSY instead of waiting).
	if _, err := tx.ExecContext(ctx, `UPDATE audio_tracks SET file_path = file_path WHERE id < 0`); err != nil {
		return 0, fmt.Errorf("acquire the write lock: %w", err)
	}

	updates := make([]store.TrackPathUpdate, 0, len(moves))
	files := make([]store.OrganiseFileRecord, 0, len(moves))
	for _, m := range moves {
		updates = append(updates, store.TrackPathUpdate{
			TrackID: m.trackID,
			From:    m.source,
			To:      m.destination,
		})
		files = append(files, store.OrganiseFileRecord{
			TrackID:         m.trackID,
			SourcePath:      m.source,
			DestinationPath: m.destination,
			Result:          store.OrganiseResultRenamed,
		})
	}
	if err := store.UpdateTrackPathsTx(ctx, tx, updates); err != nil {
		return 0, err
	}
	runID, err := store.InsertOrganiseRunTx(ctx, tx, store.OrganiseRunRecord{
		PlanHash:  plan.hash,
		FileCount: len(moves),
		Status:    store.OrganiseStatusApplied,
		Detail:    "template_change",
		Files:     files,
	})
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return runID, nil
}

// recordRun writes an audit run in its own transaction. A failure to audit is
// logged but must not hide the outcome from the operator.
func (s *Service) recordRun(ctx context.Context, plan *organisePlan, files []store.OrganiseFileRecord, status, detail string) (int64, error) {
	runID, err := s.store.InsertOrganiseRun(ctx, store.OrganiseRunRecord{
		PlanHash:  plan.hash,
		FileCount: len(files),
		Status:    status,
		Detail:    detail,
		Files:     files,
	})
	if err != nil {
		slog.Error("vault organise: could not write the audit run", "status", status, "error", err)
	}
	return runID, err
}

// recordRolledBack audits a failed apply whose renames were fully undone.
func (s *Service) recordRolledBack(ctx context.Context, plan *organisePlan, renamed []move, detail string) {
	files := make([]store.OrganiseFileRecord, 0, len(plan.moves))
	done := make(map[int64]bool, len(renamed))
	for _, m := range renamed {
		done[m.trackID] = true
	}
	for _, m := range plan.moves {
		result := store.OrganiseResultFailed
		if done[m.trackID] {
			result = store.OrganiseResultRenamedBack
		}
		files = append(files, store.OrganiseFileRecord{
			TrackID:         m.trackID,
			SourcePath:      m.source,
			DestinationPath: m.destination,
			Result:          result,
		})
	}
	if _, err := s.recordRun(ctx, plan, files, store.OrganiseStatusRolledBack, detail); err != nil {
		slog.Error("vault organise: rolled_back run was not recorded", "error", err)
	}
}

// reconcile is the last resort: the renames could not be undone, so the
// database is pointed at whatever is actually on disk and the operator is told
// loudly which paths to inspect. It never deletes or rewrites a file.
//
// attempted are the moves this apply actually made; untouched are the planned
// moves that never started, which are audited as failed because nothing moved
// them and nothing is in doubt about them.
func (s *Service) reconcile(ctx context.Context, plan *organisePlan, attempted, untouched []move, detail string) (RunResponse, *organiseFailure) {
	files := make([]store.OrganiseFileRecord, 0, len(attempted)+len(untouched))
	inspect := make([]string, 0, len(attempted))

	for _, m := range attempted {
		result := store.OrganiseResultFailed
		actual := m.source
		if _, err := os.Lstat(m.destination); err == nil {
			// The file is at its destination: the database follows the disk.
			actual = m.destination
			current, pathErr := s.store.TrackPath(ctx, m.trackID)
			switch {
			case pathErr != nil:
				slog.Error("vault organise: reconcile could not read the track path",
					"track_id", m.trackID, "error", pathErr)
			case current != actual:
				if err := s.updateTrackPath(ctx, m.trackID, current, actual); err != nil {
					slog.Error("vault organise: reconcile could not update the track path",
						"track_id", m.trackID, "error", err)
				} else {
					result = store.OrganiseResultReconciled
				}
			default:
				result = store.OrganiseResultReconciled
			}
		} else if _, err := os.Lstat(m.source); err == nil {
			// The rename-back worked for this file.
			result = store.OrganiseResultRenamedBack
		}
		if result != store.OrganiseResultRenamedBack {
			inspect = append(inspect, actual)
		}
		files = append(files, store.OrganiseFileRecord{
			TrackID:         m.trackID,
			SourcePath:      m.source,
			DestinationPath: m.destination,
			Result:          result,
		})
	}

	for _, m := range untouched {
		if _, err := os.Lstat(m.destination); err == nil {
			// Nothing should be at the destination of a move never made.
			inspect = append(inspect, m.destination)
		}
		files = append(files, store.OrganiseFileRecord{
			TrackID:         m.trackID,
			SourcePath:      m.source,
			DestinationPath: m.destination,
			Result:          store.OrganiseResultFailed,
		})
	}

	runID, runErr := s.recordRun(ctx, plan, files, store.OrganiseStatusNeedsAttention, detail)
	if runErr == nil {
		s.plans.drop(plan.hash)
	}
	slog.Error("vault organise: apply failed and could not be undone; the database now reflects the files on disk",
		"run_id", runID, "files", inspect)

	return RunResponse{RunID: runID, Status: store.OrganiseStatusNeedsAttention, FileCount: len(files)}, &organiseFailure{
		status:  http.StatusInternalServerError,
		code:    codeManualAttention,
		files:   inspect,
		message: "the rename partly failed and could not be undone; the library now reflects the files on disk — check these paths",
	}
}

// updateTrackPath writes one reconciled path, guarded on the value it read.
func (s *Service) updateTrackPath(ctx context.Context, trackID int64, from, to string) error {
	tx, err := s.store.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed
	if err := store.UpdateTrackPathsTx(ctx, tx, []store.TrackPathUpdate{{TrackID: trackID, From: from, To: to}}); err != nil {
		return err
	}
	return tx.Commit()
}

func decodePreviewRequest(r *http.Request) (previewRequest, *organiseFailure) {
	var req previewRequest
	if err := decodeBody(r, &req); err != nil {
		return req, &organiseFailure{status: http.StatusBadRequest, code: codeInvalidRequest, message: err.Error()}
	}
	return req, nil
}

func decodeApplyRequest(r *http.Request) (applyRequest, *organiseFailure) {
	var req applyRequest
	if err := decodeBody(r, &req); err != nil {
		return req, &organiseFailure{status: http.StatusBadRequest, code: codeInvalidRequest, message: err.Error()}
	}
	if strings.TrimSpace(req.PlanID) == "" {
		return req, &organiseFailure{status: http.StatusBadRequest, code: codeInvalidRequest, message: "plan_id is required"}
	}
	return req, nil
}

// decodeBody reads one small JSON object. An empty body is an empty object, so
// a preview of everything needs no payload.
func decodeBody(r *http.Request, dst any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("could not read the request body: %w", err)
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("invalid request body: %w", err)
	}
	return nil
}
