// Package queue exposes the M2 approval queue: pending donor candidates with
// their matched arr target, donor pairing and Processing Gate verdict, plus the
// approve/reject status transitions.
//
// M2 is approval-only: approve marks the row extracting for the M3 extraction
// worker; no ffmpeg/mkvmerge work happens here.
package queue

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Subject412/dubarr/backend/internal/arr"
	"github.com/Subject412/dubarr/backend/internal/settings"
	"github.com/Subject412/dubarr/backend/internal/store"
)

// defaultDonorPatterns pairs a target with sibling releases whose filename
// carries the configured dubbing marker. It matches the fixture drop
// ("Minions ... PL.DUB ... mp4" next to a tag-eligible target).
var defaultDonorPatterns = []string{"PL.DUB"}

// Dependencies are the collaborators the queue API needs.
type Dependencies struct {
	DB       *sql.DB
	Settings *settings.Store
	// Secrets decrypts arr_instances.api_key_enc for the identify route. Nil
	// (no secret store wired) makes that route answer 409 "not_configured"
	// rather than parsing unauthenticated.
	Secrets Decrypter
	// IdentifyTimeout bounds one /api/v3/parse call. Zero uses identifyTimeout
	// (30 s); tests wire a short one.
	IdentifyTimeout time.Duration
	// RequireAdmin wraps every route. When nil the API answers 503 rather than
	// running unauthenticated.
	RequireAdmin func(http.Handler) http.Handler
}

// Service holds the resolved dependencies for the registered handlers.
type Service struct {
	store           *store.Store
	settings        *settings.Store
	secrets         Decrypter
	identifyTimeout time.Duration
	requireAdmin    func(http.Handler) http.Handler
}

// RegisterRoutes mounts:
//
//	GET  /api/queue                 pending_approval candidates
//	GET  /api/imports/search-library  manual arr_library_cache search
//	POST /api/queue/{id}/approve    pending_approval -> extracting
//	POST /api/queue/{id}/reject     pending_approval -> ignored
//	POST /api/queue/{id}/identify   parse the release with arr, list candidates
//	POST /api/queue/{id}/match      persist the operator's library pick
func RegisterRoutes(mux *http.ServeMux, deps Dependencies) *Service {
	svc := &Service{
		store:           &store.Store{DB: deps.DB},
		settings:        deps.Settings,
		secrets:         deps.Secrets,
		identifyTimeout: deps.IdentifyTimeout,
		requireAdmin:    deps.RequireAdmin,
	}
	admin := func(h http.HandlerFunc) http.Handler {
		if svc.requireAdmin == nil {
			return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeError(w, http.StatusServiceUnavailable, "authorization is not configured")
			})
		}
		return svc.requireAdmin(h)
	}
	mux.Handle("GET /api/queue", admin(svc.handleList))
	mux.Handle("GET /api/imports/search-library", admin(svc.handleSearchLibrary))
	mux.Handle("POST /api/queue/{id}/approve", admin(svc.handleApprove))
	mux.Handle("POST /api/queue/{id}/reject", admin(svc.handleReject))
	mux.Handle("POST /api/queue/{id}/identify", admin(svc.handleIdentify))
	mux.Handle("POST /api/queue/{id}/match", admin(svc.handleMatch))
	return svc
}

// DonorCandidate is a sibling file that can serve as the donor for a target.
type DonorCandidate struct {
	Filename string `json:"filename"`
	Path     string `json:"path"`
	Size     int64  `json:"file_size"`
	// Selected marks the first matched donor: the card arrives pre-selected.
	Selected bool `json:"selected"`
}

// QueueItem is one approval card.
type QueueItem struct {
	ID               int64            `json:"id"`
	SourceFile       string           `json:"source_file_path"`
	Filename         string           `json:"filename"`
	FileSize         int64            `json:"file_size"`
	Status           string           `json:"status"`
	Title            string           `json:"title"`
	Year             int              `json:"year,omitempty"`
	MediaType        string           `json:"media_type,omitempty"`
	SeasonNumber     *int             `json:"season_number,omitempty"`
	EpisodeNumber    *int             `json:"episode_number,omitempty"`
	TargetPath       string           `json:"target_path"`
	OriginalLanguage string           `json:"original_language,omitempty"`
	AudioLanguages   []string         `json:"audio_languages"`
	Eligibility      string           `json:"eligibility"`
	MatchedTags      []string         `json:"matched_tags"`
	MissingTags      []string         `json:"missing_tags"`
	DonorCandidates  []DonorCandidate `json:"donor_candidates"`
	// Matched reports whether the row carries an arr library identity; when
	// false the UI must ask the operator to pick one via search-library.
	Matched      bool   `json:"matched"`
	LibraryTitle string `json:"library_title"`
	// MatchState is the operator-facing split of Matched: unmatched (no arr
	// target), matched_no_donor (a target exists but the row is not actionable)
	// or ready (approvable right now). Invariant: MatchState is
	// MatchStateUnmatched iff Matched is false.
	MatchState string `json:"match_state"`
	// MatchReason is short human prose for the UI explaining MatchState.
	MatchReason string `json:"match_reason"`
}

// Match states reported in QueueItem.MatchState.
const (
	// MatchStateUnmatched means the release has no arr target at all.
	MatchStateUnmatched = "unmatched"
	// MatchStateMatchedNoDonor means a target exists but the row is not
	// actionable: matched yet not approvable. MatchReason carries the actual
	// blocker — a missing donor candidate or a Processing Gate verdict that
	// makes the target ineligible.
	MatchStateMatchedNoDonor = "matched_no_donor"
	// MatchStateReady means the row is approvable right now: an arr target
	// exists, the target is gate-eligible and at least one donor candidate is
	// available.
	MatchStateReady = "ready"
)

// deriveMatchState splits a loaded queue row into the tri-state match state and
// the reason shown to the operator. It reads only what handleList already has:
// the arr match flag, the target path, the gate verdict with its recorded tag
// lists, and the donor listing of this row (with that listing's error) — no
// extra query.
//
// ready means approvable now, so it requires an arr match, a gate-eligible
// target and at least one donor candidate. That is a superset of the
// preconditions handleApprove derives from the row (a match, a target path and
// a paired donor): deriveMatchState is deliberately stricter, because an
// ineligible target must never be advertised as ready even though approve would
// accept it. Approve also rejects cases deriveMatchState cannot see from the
// row — a client-supplied library_cache_id that disagrees with the row's match,
// and a status race that moves the row out of pending before the transition. A
// row that is matched but fails either precondition reports matched_no_donor
// (matched, not actionable) and the reason names the actual blocker. The
// invariant "unmatched iff !matched" still holds: only the !matched case
// reports unmatched.
//
// The reason never claims a cause the row does not prove. A waiting_for_tags
// verdict has several causes (a missing required tag, an excluded tag, a
// missing arr tag, a target outside the root-folder allowlist), and a donor-less
// row has two (no matching sibling file, or a folder that could not be read at
// all), so each branch words only what it can see.
func deriveMatchState(row store.ImportCandidateSummary, donors []DonorCandidate, donorErr error, patterns []string) (state, reason string) {
	if !row.Matched {
		return MatchStateUnmatched, "No arr title matched this release."
	}
	hasTarget := strings.TrimSpace(row.TargetPath) != ""
	eligible := hasTarget && row.Eligibility == "eligible"
	if eligible && len(donors) > 0 {
		return MatchStateReady, "Ready to approve."
	}
	state = MatchStateMatchedNoDonor
	switch {
	case !hasTarget:
		return state, "The matched title has no target file to pair a donor with."
	case row.Eligibility == "waiting_for_tags":
		return state, waitingForTagsReason(row)
	case row.Eligibility == "missing_target":
		return state, "Target is not eligible for processing (target file is missing)."
	case !eligible:
		return state, "Target is not eligible for processing (the gate has not approved it)."
	case donorErr != nil:
		// The folder itself is unreadable (unmounted library, permission
		// denial): don't claim anything about the siblings it would contain.
		return state, "The target folder could not be read - check the library mount."
	case !hasDonorPattern(patterns):
		// Nothing to look for, so a marker claim would be empty prose.
		return state, "No sibling file is eligible as a donor."
	default:
		// Eligible target, readable folder, a configured marker, no match.
		return state, "No sibling release carries the dubbing marker" + markerHint(patterns) + "."
	}
}

// waitingForTagsReason words a waiting_for_tags verdict from the cause the
// recorded gate verdict proves.
//
// A gate-recorded verdict classified the target filename only, so its tag lists
// are authoritative: recorded MissingTags prove a missing required tag, while a
// waiting verdict with nothing missing can only come from an excluded tag
// (excluded tags always win). A row without a gate record took its eligibility
// from the arr library row, where arrsync writes waiting_for_tags for a missing
// arr tag or a target outside the root-folder allowlist — none of which the row
// can tell apart, so the wording stays cause-neutral.
func waitingForTagsReason(row store.ImportCandidateSummary) string {
	if !row.HasGate || row.Gate.Status != "waiting_for_tags" {
		return "The processing gate has not approved this target (waiting for tags)."
	}
	if missing := nonBlankValues(row.Gate.MissingTags); len(missing) > 0 {
		return "Target is not eligible for processing (missing required target tags: " +
			strings.Join(missing, ", ") + ")."
	}
	return "Target is not eligible for processing (the target carries an excluded tag)."
}

// nonBlankValues drops blank entries so a stray empty pattern or tag never
// counts as content.
func nonBlankValues(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// hasDonorPattern reports whether at least one non-blank donor pattern is
// configured, i.e. whether a marker claim is meaningful at all.
func hasDonorPattern(patterns []string) bool {
	return len(nonBlankValues(patterns)) > 0
}

// markerHint renders the configured donor patterns for MatchReason, e.g.
// " (PL.DUB)". It returns "" when no pattern is configured.
func markerHint(patterns []string) string {
	names := nonBlankValues(patterns)
	if len(names) == 0 {
		return ""
	}
	return " (" + strings.Join(names, ", ") + ")"
}

type listResponse struct {
	Items []QueueItem `json:"items"`
	// Diagnostics explains an empty list: whether any arr instance is
	// configured at all, whether it has ever been synced and whether the last
	// pass failed. Existing fields stay untouched; this is a sibling key.
	Diagnostics queueDiagnostics `json:"diagnostics"`
}

// queueDiagnostics tells an empty approval queue apart from an unconfigured,
// never-synced or failing appliance.
//
// Every value comes from ONE ArrLibraryHealth read — the same store read the
// library health endpoint uses — plus the arr.connections union the library
// endpoint also applies, so the queue issues no HTTP call to arr and no extra
// query per field. A freshly saved connection only gets its arr_instances row
// on the first poll (arrsync.ensureInstance), so without that union "configured
// but never synced" would be indistinguishable from "no instances configured".
type queueDiagnostics struct {
	// InstancesConfigured counts the merged arr instances: arr_instances rows
	// plus configured arr.connections entries not yet polled, deduped by kind
	// like the library health payload.
	InstancesConfigured int64 `json:"instances_configured"`
	// NeverSynced counts instances with no recorded attempt (last_attempt_at
	// NULL): configured, but no sync pass has ever run.
	NeverSynced int64 `json:"never_synced"`
	// SyncFailed counts instances whose last attempt failed (last_sync_ok 0).
	SyncFailed int64 `json:"sync_failed"`
	// LastPassAt is the most recent SUCCESSFUL sync attempt across instances,
	// RFC 3339 UTC, or null when nothing has ever succeeded. A failed attempt
	// is not a pass, and a failure leaves last_sync_at (the last success)
	// untouched, so an instance that only ever failed leaves this null.
	LastPassAt *string `json:"last_pass_at"`
	// CacheItems is the total cached arr library rows across all instances and
	// both media kinds: synced but nothing matched shows up as a non-zero count
	// with an empty queue.
	CacheItems int64 `json:"cache_items"`
}

// syncTimestampLayout is how the store writes arr_instances timestamps
// (arr_library_sync.go): UTC, second precision, sortable as plain text.
const syncTimestampLayout = "2006-01-02 15:04:05"

// zeroedQueueDiagnostics is the shape emitted when the health read fails: the
// queue must stay usable when the library cache is unreadable, so diagnostics
// degrade to "nothing known" instead of failing the list.
func zeroedQueueDiagnostics() queueDiagnostics {
	return queueDiagnostics{LastPassAt: nil}
}

// queueDiagnosticsFor derives the diagnostics from the health rows, the
// reported cache total and the configured connection kinds.
func queueDiagnosticsFor(rows []store.ArrInstanceHealth, cacheItems int64, configuredKinds []string) queueDiagnostics {
	diag := queueDiagnostics{CacheItems: cacheItems}
	seen := make(map[string]struct{}, len(rows)+len(configuredKinds))
	// Timestamps compare as text: the store writes one fixed sortable layout.
	lastPass := ""
	for _, row := range rows {
		seen[row.Kind] = struct{}{}
		diag.InstancesConfigured++
		switch {
		case !row.LastAttemptAt.Valid:
			diag.NeverSynced++
		case row.LastOK.Valid && !row.LastOK.Bool:
			// A failure is not a pass: last_sync_at still holds the last
			// success (or NULL when there never was one).
			diag.SyncFailed++
		}
		if row.LastSyncAt.Valid && row.LastSyncAt.String > lastPass {
			lastPass = row.LastSyncAt.String
		}
	}
	// Configured-but-unpolled connections: no arr_instances row yet, so no
	// attempt and nothing cached — the never-synced shape.
	for _, kind := range configuredKinds {
		if _, ok := seen[kind]; ok {
			continue
		}
		seen[kind] = struct{}{}
		diag.InstancesConfigured++
		diag.NeverSynced++
	}
	if lastPass != "" {
		formatted := formatSyncTimestamp(lastPass)
		diag.LastPassAt = &formatted
	}
	return diag
}

// formatSyncTimestamp renders a stored timestamp as RFC 3339 UTC so the UI can
// parse it as a time. A value in an unexpected layout is passed through rather
// than dropped, so no history is invented or lost.
func formatSyncTimestamp(raw string) string {
	ts, err := time.ParseInLocation(syncTimestampLayout, raw, time.UTC)
	if err != nil {
		return raw
	}
	return ts.UTC().Format(time.RFC3339)
}

// diagnostics reads the sync health of the library cache for the queue list.
// A store error is logged and zeroed: an unreadable library cache must not
// make the approval queue unavailable.
func (s *Service) diagnostics(ctx context.Context) queueDiagnostics {
	rows, cacheItems, err := s.store.ArrLibraryHealth(ctx)
	if err != nil {
		slog.Default().Warn("queue list: read arr sync health",
			"component", "queue", "error", err)
		return zeroedQueueDiagnostics()
	}
	return queueDiagnosticsFor(rows, cacheItems, s.configuredArrKinds(ctx))
}

// configuredArrKinds returns the arr kinds saved in arr.connections, via the
// same union the library health endpoint applies (arr.ConfiguredConnections),
// so "configured" cannot mean two different things on two screens. A read or
// decode failure is logged and swallowed for the same reason as a health read
// failure: diagnostics are never allowed to break the queue.
func (s *Service) configuredArrKinds(ctx context.Context) []string {
	conns, err := arr.ConfiguredConnections(ctx, s.settings)
	if err != nil {
		slog.Default().Warn("queue list: read arr connections",
			"component", "queue", "error", err)
		return nil
	}
	kinds := make([]string, 0, len(conns))
	for _, conn := range conns {
		kinds = append(kinds, conn.Kind)
	}
	return kinds
}

type transitionResponse struct {
	ID     int64  `json:"id"`
	Status string `json:"status"`
}

func (s *Service) handleList(w http.ResponseWriter, r *http.Request) {
	if s.store == nil || s.store.DB == nil {
		writeError(w, http.StatusServiceUnavailable, "queue store is not configured")
		return
	}
	rows, err := s.store.ListPendingImportCandidates(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read the approval queue")
		return
	}
	patterns := s.donorPatterns(r.Context())
	items := make([]QueueItem, 0, len(rows))
	for _, row := range rows {
		donors, donorErr := pairDonors(row.TargetPath, patterns)
		matchState, matchReason := deriveMatchState(row, donors, donorErr, patterns)
		item := QueueItem{
			ID:               row.ID,
			SourceFile:       row.SourceFilePath,
			Filename:         row.Filename,
			FileSize:         row.FileSize,
			Status:           row.Status,
			Title:            row.Title,
			Year:             row.Year,
			MediaType:        row.MediaType,
			SeasonNumber:     row.SeasonNumber,
			EpisodeNumber:    row.EpisodeNumber,
			TargetPath:       row.TargetPath,
			OriginalLanguage: row.OriginalLanguage,
			AudioLanguages:   decodeLanguages(row.AudioLanguages),
			Eligibility:      row.Eligibility,
			MatchedTags:      row.Gate.MatchedTags,
			MissingTags:      row.Gate.MissingTags,
			DonorCandidates:  donors,
			Matched:          row.Matched,
			LibraryTitle:     row.LibraryTitle,
			MatchState:       matchState,
			MatchReason:      matchReason,
		}
		// Post-approval rows leave the list, but when a donor was persisted
		// (e.g. re-listing before extraction) the card must agree with it.
		if row.DonorPath != "" {
			for i := range item.DonorCandidates {
				item.DonorCandidates[i].Selected = item.DonorCandidates[i].Path == row.DonorPath
			}
		}
		if item.MatchedTags == nil {
			item.MatchedTags = []string{}
		}
		if item.MissingTags == nil {
			item.MissingTags = []string{}
		}
		items = append(items, item)
	}
	writeJSON(w, http.StatusOK, listResponse{Items: items, Diagnostics: s.diagnostics(r.Context())})
}

// handleSearchLibrary serves the manual-match lookup: a case-insensitive title
// substring search over the arr-synced library cache (IDEA.md §4), used when
// the automatic /api/v3/parse match failed (e.g. localized release names).
func (s *Service) handleSearchLibrary(w http.ResponseWriter, r *http.Request) {
	if s.store == nil || s.store.DB == nil {
		writeError(w, http.StatusServiceUnavailable, "queue store is not configured")
		return
	}
	items, err := s.store.ListLibrarySearch(r.Context(), r.URL.Query().Get("q"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not search the library")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// approveRequest is the optional approve body. An empty or absent body keeps
// the arr-matched behaviour; library_cache_id supplies a manual match for a
// queue row the automatic parse could not identify.
type approveRequest struct {
	LibraryCacheID int64 `json:"library_cache_id"`
}

// decodeApproveBody tolerates an absent/empty body and rejects malformed JSON
// so a bad request never looks like "no manual match".
func decodeApproveBody(r *http.Request) (approveRequest, error) {
	var req approveRequest
	if r.Body == nil {
		return req, nil
	}
	err := json.NewDecoder(r.Body).Decode(&req)
	if errors.Is(err, io.EOF) {
		return req, nil
	}
	return req, err
}

func (s *Service) handleApprove(w http.ResponseWriter, r *http.Request) {
	if s.store == nil || s.store.DB == nil {
		writeError(w, http.StatusServiceUnavailable, "queue store is not configured")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid queue id")
		return
	}
	req, err := decodeApproveBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.LibraryCacheID < 0 {
		writeError(w, http.StatusBadRequest, "invalid library_cache_id")
		return
	}

	// Resolve the donor exactly as the approval card does, then persist it with
	// the status transition so the extractor never has to re-pair (and never
	// mistakes the candidate file for the donor).
	target, currentMatch, err := s.store.ImportCandidateMatchTarget(r.Context(), id)
	if err != nil {
		writeQueueMutationError(w, err, "could not read the queue item")
		return
	}

	// Extraction→vault needs a media identity. A row without one may be
	// matched manually here; otherwise approve must not proceed.
	if currentMatch == 0 && req.LibraryCacheID == 0 {
		writeError(w, http.StatusConflict, "no matched library target")
		return
	}
	if req.LibraryCacheID != 0 {
		if currentMatch != 0 && currentMatch != req.LibraryCacheID {
			writeError(w, http.StatusConflict, "library target does not match the queue item's matched library")
			return
		}
		cacheTarget, ok, err := s.store.LibraryCacheTarget(r.Context(), req.LibraryCacheID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "could not read the library target")
			return
		}
		if !ok {
			writeError(w, http.StatusNotFound, "library target not found")
			return
		}
		if strings.TrimSpace(target) == "" {
			target = cacheTarget
		}
	}
	if strings.TrimSpace(target) == "" {
		writeError(w, http.StatusConflict, "matched library target has no target file to pair a donor with")
		return
	}
	// Pair the donor exactly as the list endpoint does. The listing error is a
	// different failure from "no matching sibling": an unreadable folder must
	// not be reported as a missing donor.
	donors, donorErr := pairDonors(target, s.donorPatterns(r.Context()))
	if donorErr != nil {
		writeError(w, http.StatusConflict, "the target folder could not be read - check the library mount")
		return
	}
	if len(donors) == 0 {
		writeError(w, http.StatusConflict, "no donor paired: no sibling file is eligible as a donor")
		return
	}
	if err := s.store.SetImportCandidateApproval(r.Context(), id, donors[0].Path, req.LibraryCacheID); err != nil {
		writeQueueMutationError(w, err, "could not update the queue item")
		return
	}
	writeJSON(w, http.StatusOK, transitionResponse{ID: id, Status: "extracting"})
}

// writeQueueMutationError maps store sentinels shared by approve and the status
// transitions onto HTTP statuses.
func writeQueueMutationError(w http.ResponseWriter, err error, message string) {
	switch {
	case errors.Is(err, store.ErrImportCandidateNotFound), errors.Is(err, store.ErrLibraryCacheNotFound):
		writeError(w, http.StatusNotFound, "queue item not found")
	case errors.Is(err, store.ErrImportCandidateState):
		writeError(w, http.StatusConflict, "queue item is no longer pending approval")
	case errors.Is(err, store.ErrImportCandidateMatchMismatch):
		writeError(w, http.StatusConflict, "library target does not match the queue item's matched library")
	default:
		writeError(w, http.StatusInternalServerError, message)
	}
}

func (s *Service) handleReject(w http.ResponseWriter, r *http.Request) {
	s.transition(w, r, "ignored")
}

func (s *Service) transition(w http.ResponseWriter, r *http.Request, to string) {
	if s.store == nil || s.store.DB == nil {
		writeError(w, http.StatusServiceUnavailable, "queue store is not configured")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid queue id")
		return
	}
	if err := s.store.TransitionImportCandidate(r.Context(), id, to); err != nil {
		switch {
		case errors.Is(err, store.ErrImportCandidateNotFound):
			writeError(w, http.StatusNotFound, "queue item not found")
		case errors.Is(err, store.ErrImportCandidateState):
			writeError(w, http.StatusConflict, "queue item is no longer pending approval")
		default:
			writeError(w, http.StatusInternalServerError, "could not update the queue item")
		}
		return
	}
	writeJSON(w, http.StatusOK, transitionResponse{ID: id, Status: to})
}

// donorPatterns reads imports.options from the settings kv, falling back to the
// fixture-compatible default when the section is missing or unset.
func (s *Service) donorPatterns(ctx context.Context) []string {
	if s.settings == nil {
		return defaultDonorPatterns
	}
	raw, ok, err := s.settings.Get(ctx, settings.KeyImportsOptions)
	if err != nil || !ok {
		return defaultDonorPatterns
	}
	var opts settings.ImportsOptions
	if err := json.Unmarshal(raw, &opts); err != nil || len(opts.DonorPatterns) == 0 {
		return defaultDonorPatterns
	}
	return opts.DonorPatterns
}

// pairDonors lists regular sibling files of targetPath whose basename matches
// one of patterns. The first match (name order) is marked selected so the
// approval card arrives pre-selected.
//
// A non-nil error means the containing folder could not be listed at all
// (unmounted library, permission denial), which is not the same as listing it
// and finding no matching sibling: callers must word the two cases differently
// instead of blaming the absence of a donor. An empty target path or an empty
// pattern list is not an error — there is simply nothing to pair.
func pairDonors(targetPath string, patterns []string) ([]DonorCandidate, error) {
	out := []DonorCandidate{}
	if strings.TrimSpace(targetPath) == "" || len(patterns) == 0 {
		return out, nil
	}
	dir := filepath.Dir(targetPath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out, fmt.Errorf("queue: list donor folder %s: %w", dir, err)
	}
	targetName := filepath.Base(targetPath)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || entry.Name() == targetName {
			continue
		}
		if matchesDonorPattern(entry.Name(), patterns) {
			names = append(names, entry.Name())
		}
	}
	sort.Slice(names, func(i, j int) bool {
		return strings.ToLower(names[i]) < strings.ToLower(names[j])
	})
	for i, name := range names {
		full := filepath.Join(dir, name)
		var size int64
		if info, err := os.Stat(full); err == nil {
			size = info.Size()
		}
		out = append(out, DonorCandidate{
			Filename: name,
			Path:     full,
			Size:     size,
			Selected: i == 0,
		})
	}
	return out, nil
}

// matchesDonorPattern reports whether filename matches any pattern. Matching is
// case-insensitive. A pattern containing '*' is treated as a glob, otherwise it
// is a substring match, so the default "PL.DUB" finds release names reliably.
func matchesDonorPattern(filename string, patterns []string) bool {
	lower := strings.ToLower(filename)
	for _, pattern := range patterns {
		p := strings.ToLower(strings.TrimSpace(pattern))
		if p == "" {
			continue
		}
		if strings.Contains(p, "*") {
			if ok, err := path.Match(p, lower); err == nil && ok {
				return true
			}
			continue
		}
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

func decodeLanguages(raw string) []string {
	out := []string{}
	if strings.TrimSpace(raw) == "" {
		return out
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil || out == nil {
		return []string{}
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
