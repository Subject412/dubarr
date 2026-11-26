// The per-item rescan endpoint (#29): ask Radarr/Sonarr to rescan the item
// behind one Library row, then re-read that item so the cache reflects what
// arr now holds.
//
// The command is accepted asynchronously by arr, so the honest answer is two
// facts, not one: the command was accepted (202) and whether the follow-up
// re-read actually succeeded ("reread": "ok"|"failed"). A failed re-read never
// fails the request — the rescan did happen, and hiding that behind a 5xx would
// invite a second, duplicated command.
//
// The cache write goes through store.UpdateLibraryCacheItem, which upserts the
// observed row only. The snapshot semantics of SyncLibraryCache (delete the
// rows a pass did not see) must never apply here: this route observed one item,
// not the instance's library.
package library

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Subject412/dubarr/backend/internal/arr"
	"github.com/Subject412/dubarr/backend/internal/gate"
	"github.com/Subject412/dubarr/backend/internal/logs"
	"github.com/Subject412/dubarr/backend/internal/probe"
	"github.com/Subject412/dubarr/backend/internal/settings"
	"github.com/Subject412/dubarr/backend/internal/store"
)

// rereadErrorLimit caps the scrubbed re-read cause returned to the operator, so
// a pathological decoder/driver message cannot bloat the payload.
const rereadErrorLimit = 300

// Decrypter is the subset of secrets.Service the rescan route uses: it decrypts
// the API key the sync pass already stored in arr_instances. A nil Decrypter
// (no secret store wired) makes the route answer 409 "not_configured".
type Decrypter interface {
	DecryptString(blob []byte) (string, error)
}

// rescanResponse is the 202 body: the command was accepted, plus the honest
// verdict of the re-read that followed it.
type rescanResponse struct {
	Accepted bool `json:"accepted"`
	// Reread is "ok" when the cache row was refreshed, "failed" otherwise.
	Reread string `json:"reread"`
	// RereadError is the scrubbed cause, present only when Reread is "failed".
	RereadError string `json:"reread_error,omitempty"`
}

// arrMetadataSource is the read surface a re-read needs beyond the item itself:
// both products expose the same tag and root-folder endpoints.
type arrMetadataSource interface {
	Tags(ctx context.Context) ([]arr.Tag, error)
	RootFolders(ctx context.Context) ([]arr.RootFolder, error)
}

// handleRescan issues one arr rescan command for a cache row and then re-reads
// that row.
//
//	202 {"accepted": true, "reread": "ok"}
//	202 {"accepted": true, "reread": "failed", "reread_error": "..."}
//	400 the {id} is not a positive integer
//	404 the id names no cache row
//	409 code "not_configured"   no secret store, no enabled instance, no key
//	502 code "rescan_failed"    arr refused the command or could not be reached
func (s *Service) handleRescan(w http.ResponseWriter, r *http.Request) {
	if s.store == nil || s.store.DB == nil {
		writeError(w, http.StatusServiceUnavailable, "library store is not configured")
		return
	}
	ctx := r.Context()
	id, err := strconv.ParseInt(strings.TrimSpace(r.PathValue("id")), 10, 64)
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, "invalid library item id")
		return
	}

	item, err := s.store.LibraryItemByID(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrLibraryItemNotFound) {
			writeError(w, http.StatusNotFound, "library item not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "could not read the library item")
		return
	}

	step, reason, err := s.planRescan(ctx, item)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read the arr instance")
		return
	}
	if reason != "" {
		writeRescanNotConfigured(w, reason)
		return
	}

	// 1. The command. Its failure is the only 502: nothing was accepted.
	if err := step.command(ctx); err != nil {
		writeJSON(w, http.StatusBadGateway, errorBody{
			Error: rescanFailureMessage(err),
			Code:  "rescan_failed",
		})
		return
	}

	// 2. The re-read, best effort. A cache that lags arr is worth reporting but
	// is not a failed rescan: the command is already queued in arr.
	resp := rescanResponse{Accepted: true, Reread: "ok"}
	if err := step.reread(ctx); err != nil {
		resp.Reread = "failed"
		resp.RereadError = truncateRereadError(logs.Redact(err.Error()))
	}
	writeJSON(w, http.StatusAccepted, resp)
}

// rescanStep is the pair of arr calls one rescan makes: the command, and the
// re-read of the same item. Both close over the client built once here, so the
// route never constructs a second one.
type rescanStep struct {
	command func(context.Context) error
	reread  func(context.Context) error
}

// planRescan resolves the row's instance and secret and builds the two calls. A
// non-empty reason means the row cannot be rescanned as configured, and the
// caller answers 409 not_configured with it; a returned error is a store fault.
//
// The client is built exactly like the poller's: the instance row the cache
// points at supplies the base URL and the encrypted key, and the key is only
// ever handed to the arr client.
func (s *Service) planRescan(ctx context.Context, item store.LibraryListItem) (rescanStep, string, error) {
	if s.secrets == nil {
		return rescanStep{}, "no arr secret store is configured", nil
	}
	instance, ok, err := s.store.ArrInstanceByID(ctx, item.ArrInstanceID)
	if err != nil {
		return rescanStep{}, "", err
	}
	if !ok {
		return rescanStep{}, "the item's arr instance is not configured", nil
	}
	if !instance.Enabled {
		return rescanStep{}, "the item's arr instance is disabled", nil
	}
	if len(instance.APIKeyEnc) == 0 {
		return rescanStep{}, "the arr instance has no API key", nil
	}
	apiKey, err := s.secrets.DecryptString(instance.APIKeyEnc)
	if err != nil {
		return rescanStep{}, "the arr API key could not be read", nil
	}
	rule, err := s.gateRule(ctx)
	if err != nil {
		// A broken settings row must not stop the rescan command; the re-read
		// then reports its own failure instead.
		log.Printf("library: read tag filter for rescan: %v", err)
	}

	switch {
	case instance.Kind == string(arr.KindRadarr) && item.MediaType == "movie":
		if item.ExternalItemID <= 0 {
			return rescanStep{}, "the item has no arr movie id", nil
		}
		client, err := arr.NewRadarrClient(instance.BaseURL, apiKey, arr.DefaultTimeout)
		if err != nil {
			return rescanStep{}, "the arr connection is not usable", nil
		}
		movieID := int(item.ExternalItemID)
		return rescanStep{
			command: func(ctx context.Context) error { return client.RescanMovie(ctx, movieID) },
			reread:  func(ctx context.Context) error { return s.rereadMovie(ctx, client, item, rule) },
		}, "", nil
	case instance.Kind == string(arr.KindSonarr) && item.MediaType == "episode":
		if item.SeriesID <= 0 {
			return rescanStep{}, "the item has no arr series id", nil
		}
		client, err := arr.NewSonarrClient(instance.BaseURL, apiKey, arr.DefaultTimeout)
		if err != nil {
			return rescanStep{}, "the arr connection is not usable", nil
		}
		seriesID := int(item.SeriesID)
		return rescanStep{
			command: func(ctx context.Context) error { return client.RescanSeries(ctx, seriesID) },
			reread:  func(ctx context.Context) error { return s.rereadEpisode(ctx, client, item, rule) },
		}, "", nil
	default:
		return rescanStep{}, "the item's arr instance does not serve its media type", nil
	}
}

// rereadMovie re-reads one movie and upserts its cache row.
func (s *Service) rereadMovie(ctx context.Context, client *arr.RadarrClient, item store.LibraryListItem, rule gate.Rule) error {
	movie, err := client.MovieByID(ctx, int(item.ExternalItemID))
	if err != nil {
		return rereadReadError(err)
	}
	labels, roots, err := arrMetadata(ctx, client)
	if err != nil {
		return err
	}
	target := ""
	if movie.HasFile && movie.MovieFile != nil {
		target = movie.MovieFile.Path
	}
	tagIDs, tagLabels := resolveArrTags(movie.Tags, labels)
	root := rootFolderFor(movie.Path, roots)
	status, matches := classifyTarget(rule, target, gate.ArrMetadata{
		Found:      true,
		Tags:       tagLabels,
		RootFolder: root,
	})
	return s.store.UpdateLibraryCacheItem(ctx, item.ArrInstanceID, store.LibraryCacheItem{
		MediaType:            "movie",
		ExternalItemID:       int64(movie.ID),
		TMDBID:               int64(movie.TmdbID),
		Title:                movie.Title,
		OriginalTitle:        movie.OriginalTitle,
		OriginalLanguageISO2: probe.ISO639_1(movie.OriginalLanguage.Name),
		Year:                 movie.Year,
		MediaFolderPath:      movie.Path,
		RootFolderPath:       root,
		ArrTagIDs:            tagIDs,
		ArrTagLabels:         tagLabels,
		TargetFilePath:       target,
		MatchesTargetTags:    matches,
		EligibilityStatus:    status,
	})
}

// rereadEpisode re-reads one episode of a series and upserts its cache row. The
// episode is listed through its series (Sonarr has no single-episode GET in the
// surface Dubarr uses); an episode the series no longer reports is an error, not
// a silent no-op, so the operator learns the row is stale.
func (s *Service) rereadEpisode(ctx context.Context, client *arr.SonarrClient, item store.LibraryListItem, rule gate.Rule) error {
	show, err := client.SeriesByID(ctx, int(item.SeriesID))
	if err != nil {
		return rereadReadError(err)
	}
	episodes, err := client.Episodes(ctx, int(item.SeriesID))
	if err != nil {
		return rereadReadError(err)
	}
	var episode *arr.Episode
	for i := range episodes {
		if int64(episodes[i].ID) == item.ExternalItemID {
			episode = &episodes[i]
			break
		}
	}
	if episode == nil {
		return errors.New("the episode is no longer listed by arr")
	}
	labels, roots, err := arrMetadata(ctx, client)
	if err != nil {
		return err
	}
	target := ""
	if episode.HasFile && episode.EpisodeFile != nil {
		target = episode.EpisodeFile.Path
	}
	tagIDs, tagLabels := resolveArrTags(show.Tags, labels)
	root := rootFolderFor(show.Path, roots)
	status, matches := classifyTarget(rule, target, gate.ArrMetadata{
		Found:      true,
		Tags:       tagLabels,
		RootFolder: root,
	})
	season, number := episode.SeasonNumber, episode.EpisodeNumber
	return s.store.UpdateLibraryCacheItem(ctx, item.ArrInstanceID, store.LibraryCacheItem{
		MediaType:            "episode",
		ExternalItemID:       int64(episode.ID),
		SeriesID:             int64(show.ID),
		TVDBID:               int64(show.TvdbID),
		Title:                show.Title,
		OriginalTitle:        show.OriginalTitle,
		OriginalLanguageISO2: probe.ISO639_1(show.OriginalLanguage.Name),
		Year:                 show.Year,
		SeasonNumber:         &season,
		EpisodeNumber:        &number,
		EpisodeTitle:         episode.Title,
		MediaFolderPath:      show.Path,
		RootFolderPath:       root,
		ArrTagIDs:            tagIDs,
		ArrTagLabels:         tagLabels,
		TargetFilePath:       target,
		MatchesTargetTags:    matches,
		EligibilityStatus:    status,
	})
}

// errItemGoneFromArr is what a re-read reports when arr answers 404: the item
// the cache still holds no longer exists on the arr side. The raw client error
// ("arr: unexpected HTTP status 404 from GET /api/v3/movie/42") is an
// implementation detail an operator cannot act on.
var errItemGoneFromArr = errors.New("the item is no longer in arr")

// rereadReadError maps a re-read's read failure to the cause the operator sees.
// Only a 404 becomes the plain sentence; every other failure keeps the client's
// (already key-scrubbed) message, which carries the status or the network cause.
func rereadReadError(err error) error {
	var status *arr.StatusError
	if errors.As(err, &status) && status.Code == http.StatusNotFound {
		return errItemGoneFromArr
	}
	return err
}

// gateRule reads settings target.tag_filter the way the poller does: the section
// when present, the seeded default when absent, so a re-read classifies with the
// same rule the next sync would use.
func (s *Service) gateRule(ctx context.Context) (gate.Rule, error) {
	if s.settings == nil {
		return gate.Rule{}, errors.New("library: settings are not configured")
	}
	raw, ok, err := s.settings.Get(ctx, settings.KeyTargetTagFilter)
	if err != nil {
		return gate.Rule{}, err
	}
	if !ok {
		tf, _ := settings.Defaults("")[settings.KeyTargetTagFilter].(settings.TargetTagFilter)
		return gate.RuleFromTagFilter(tf), nil
	}
	var tf settings.TargetTagFilter
	if err := json.Unmarshal(raw, &tf); err != nil {
		return gate.Rule{}, err
	}
	return gate.RuleFromTagFilter(tf), nil
}

// arrMetadata reads the tags and root folders one re-read needs. Tags are
// resolved to labels here, so a renamed or deleted tag cannot survive in the
// snapshot.
func arrMetadata(ctx context.Context, client arrMetadataSource) (map[int]string, []arr.RootFolder, error) {
	tags, err := client.Tags(ctx)
	if err != nil {
		return nil, nil, err
	}
	roots, err := client.RootFolders(ctx)
	if err != nil {
		return nil, nil, err
	}
	labels := make(map[int]string, len(tags))
	for _, tag := range tags {
		labels[int(tag.ID)] = tag.Label
	}
	return labels, roots, nil
}

// resolveArrTags turns arr tag ids into the paired id/label snapshot, dropping
// ids the instance no longer lists (ADR-0009 matches by label).
func resolveArrTags(ids []int, labels map[int]string) ([]int64, []string) {
	tagIDs := make([]int64, 0, len(ids))
	tagLabels := make([]string, 0, len(ids))
	for _, id := range ids {
		label, ok := labels[id]
		if !ok {
			continue
		}
		tagIDs = append(tagIDs, int64(id))
		tagLabels = append(tagLabels, label)
	}
	return tagIDs, tagLabels
}

// rootFolderFor picks the longest configured root containing the media folder,
// compared as arr reports the paths.
func rootFolderFor(mediaFolder string, roots []arr.RootFolder) string {
	best := ""
	for _, root := range roots {
		path := strings.TrimRight(root.Path, "/")
		if path == "" {
			continue
		}
		if mediaFolder == path || strings.HasPrefix(mediaFolder, path+"/") {
			if len(path) > len(best) {
				best = path
			}
		}
	}
	return best
}

// classifyTarget turns the gate verdict into the stored status/matches pair,
// exactly as the sync does for a single item.
func classifyTarget(rule gate.Rule, targetPath string, arrMeta gate.ArrMetadata) (string, bool) {
	if targetPath == "" {
		return string(gate.StatusMissingTarget), false
	}
	res := rule.Classify(filepath.Base(targetPath), arrMeta)
	return string(res.Status), res.Status == gate.StatusEligible
}

// rescanFailureMessage maps a rescan command failure to an operator-facing
// message. The arr clients already scrub the API key from their errors, and the
// messages here stay generic enough that no credential could ride along.
func rescanFailureMessage(err error) string {
	var status *arr.StatusError
	switch {
	case errors.Is(err, arr.ErrAuth):
		return "the instance rejected the API key"
	case errors.Is(err, arr.ErrNetwork):
		return "could not reach the instance: check the base URL and network"
	case errors.As(err, &status):
		return "the instance refused the rescan command (HTTP " + strconv.Itoa(status.Code) + ")"
	default:
		return "the instance did not accept the rescan command"
	}
}

// writeRescanNotConfigured is the 409 every "cannot rescan this row" case
// shares, so the UI can tell it apart from a server fault.
func writeRescanNotConfigured(w http.ResponseWriter, message string) {
	writeJSON(w, http.StatusConflict, errorBody{Error: message, Code: "not_configured"})
}

// truncateRereadError caps the cause without splitting a multi-byte rune.
func truncateRereadError(cause string) string {
	if len(cause) <= rereadErrorLimit {
		return cause
	}
	cut := rereadErrorLimit
	for cut > 0 && !utf8.RuneStart(cause[cut]) {
		cut--
	}
	return cause[:cut]
}
