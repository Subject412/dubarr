// Identifying an unmatched release (#30, part 1b).
//
// POST /api/queue/{id}/identify asks the arr instance behind the row to parse
// the release name and offers the arr_library_cache rows that fit the verdict.
// POST /api/queue/{id}/match persists the operator's pick.
//
// Both routes are deliberately dumb about identity: identify never picks a
// candidate (the operator does), and match never approves (the approve route
// owns the status transition). Everything arr-facing goes through the existing
// clients, so the shared error model and its key scrubbing apply unchanged.
package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Subject412/dubarr/backend/internal/arr"
	"github.com/Subject412/dubarr/backend/internal/store"
)

// identifyTimeout bounds one parse round trip when no explicit timeout is
// configured on the service.
const identifyTimeout = 30 * time.Second

// identifyCandidateLimit caps how many ranked candidates the identify route
// returns. The pool it ranks is larger (store.IdentifyPoolLimit).
const identifyCandidateLimit = 20

// Decrypter is the subset of secrets.Service the identify route uses: it opens
// the API key the sync pass stored in arr_instances. A nil Decrypter (no secret
// store wired) makes the route answer 409 "not_configured".
type Decrypter interface {
	DecryptString(blob []byte) (string, error)
}

// parser is the slice of the arr clients identify needs. Both *arr.RadarrClient
// and *arr.SonarrClient satisfy it, so the route builds one client per kind and
// never a third construction path.
type parser interface {
	Parse(ctx context.Context, title string) (arr.ParseResult, error)
}

// identifyRequest is the optional identify body: an operator-supplied release
// name overriding the row's filename stem.
type identifyRequest struct {
	Title string `json:"title"`
}

// matchRequest is the match body. Confidence is accepted so an existing client
// can keep sending the identify score, but it never reaches parse_confidence: a
// human pick is not a parse, so the store always records it as "manual".
type matchRequest struct {
	LibraryCacheID int64   `json:"library_cache_id"`
	Confidence     float64 `json:"confidence"`
}

// identifyCandidate is one arr_library_cache row offered as a possible match.
type identifyCandidate struct {
	LibraryCacheID int64   `json:"library_cache_id"`
	Title          string  `json:"title"`
	Year           int     `json:"year"`
	MediaType      string  `json:"media_type"`
	ExternalItemID int64   `json:"external_item_id"`
	Confidence     float64 `json:"confidence"`
	// MatchedBy names how the candidate was found; the parse route only ever
	// reports "parse".
	MatchedBy string `json:"matched_by"`
}

// identifyParse is what arr thought of the release name. Parsed is false when
// the instance recognised no title: an unparsable release, not a cold cache.
type identifyParse struct {
	Parsed     bool    `json:"parsed"`
	Title      string  `json:"title"`
	Year       int     `json:"year"`
	Quality    string  `json:"quality"`
	Confidence float64 `json:"confidence"`
}

type identifyResponse struct {
	Parse      identifyParse       `json:"parse"`
	Candidates []identifyCandidate `json:"candidates"`
}

type matchResponse struct {
	MatchState string `json:"match_state"`
}

// apiError carries the machine-readable reason beside the human message so the
// UI can tell an unconfigured appliance from an arr failure.
type apiError struct {
	Error string `json:"error"`
	Code  string `json:"code,omitempty"`
	// Class is the arr failure class: timeout, auth, network, status or
	// unknown. Empty for every non-arr failure.
	Class string `json:"class,omitempty"`
}

// writeAPIError writes one error envelope.
func writeAPIError(w http.ResponseWriter, status int, message, code, class string) {
	writeJSON(w, status, apiError{Error: message, Code: code, Class: class})
}

// handleIdentify parses the release with arr and returns the ranked candidates.
//
//	200 {"parse": {...}, "candidates": [...]}
//	400 the {id} is not a positive integer, the body is malformed, or there is
//	    no release name to parse
//	404 the id names no queue row
//	409 code "not_configured"  no instance, key or secret store for this row
//	502 code "identify_failed" arr refused or could not answer; class says why
//
// An empty library cache is not an error: the parse verdict is still worth
// showing, and candidates come back as an empty array. A release arr could not
// parse is reported as parse.parsed=false with an empty candidate array and no
// library read at all, so "unparsable" is never mistaken for "nothing matched".
func (s *Service) handleIdentify(w http.ResponseWriter, r *http.Request) {
	if s.store == nil || s.store.DB == nil {
		writeError(w, http.StatusServiceUnavailable, "queue store is not configured")
		return
	}
	ctx := r.Context()
	id, ok := parseQueueID(w, r)
	if !ok {
		return
	}
	req, err := decodeIdentifyBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	row, err := s.store.ImportCandidateIdentify(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrImportCandidateNotFound) {
			writeError(w, http.StatusNotFound, "queue item not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "could not read the queue item")
		return
	}

	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = strings.TrimSpace(strings.TrimSuffix(row.Filename, filepath.Ext(row.Filename)))
	}
	if title == "" {
		writeError(w, http.StatusBadRequest, "no release name to parse")
		return
	}

	kind := s.identifyKind(ctx, row)
	if kind == "" {
		writeAPIError(w, http.StatusConflict, "no arr instance is configured for this release", "not_configured", "")
		return
	}
	client, reason, err := s.identifyParser(ctx, kind)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read the arr instance")
		return
	}
	if reason != "" {
		writeAPIError(w, http.StatusConflict, reason, "not_configured", "")
		return
	}

	// The parse is bounded by the caller, not by the client's own timeout: a
	// hung instance must fail the request with a class the operator can read.
	parseCtx, cancel := context.WithTimeout(ctx, s.identifyDeadline())
	defer cancel()
	parsed, err := client.Parse(parseCtx, title)
	if err != nil {
		class, message := classifyIdentifyError(err)
		writeAPIError(w, http.StatusBadGateway, message, "identify_failed", class)
		return
	}

	parseView := identifyParse{
		Parsed:     parsed.Parsed,
		Title:      parsed.Title,
		Year:       parsed.Year,
		Quality:    parsed.Quality,
		Confidence: parsed.Confidence,
	}
	// An unparsable release short-circuits before the library read: scoring an
	// empty title against the pool would scan every row and return nothing,
	// which looks identical to a healthy cold cache. Reporting parsed=false with
	// no candidates keeps the two apart without inventing a title.
	if !parsed.Parsed {
		writeJSON(w, http.StatusOK, identifyResponse{Parse: parseView, Candidates: []identifyCandidate{}})
		return
	}

	mediaType := s.identifyMediaType(row, kind)
	pool, err := s.store.IdentifyCandidatePool(ctx, row.InstanceID, mediaType, poolTerm(parsed.Title))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not search the library")
		return
	}

	writeJSON(w, http.StatusOK, identifyResponse{
		Parse:      parseView,
		Candidates: rankCandidates(parsed.Title, parsed.Year, pool),
	})
}

// handleMatch persists the operator's pick and reports the match state the queue
// list would show for the row.
//
//	200 {"match_state": "unmatched"|"matched_no_donor"|"ready"}
//	400 invalid id or body
//	404 unknown queue row or unknown candidate
//	409 already matched to a different id, the kinds disagree, or the target row
//	    belongs to another arr instance
//	422 the row is not pending_approval
func (s *Service) handleMatch(w http.ResponseWriter, r *http.Request) {
	if s.store == nil || s.store.DB == nil {
		writeError(w, http.StatusServiceUnavailable, "queue store is not configured")
		return
	}
	ctx := r.Context()
	id, ok := parseQueueID(w, r)
	if !ok {
		return
	}
	req, err := decodeMatchBody(r)
	if err != nil || req.LibraryCacheID <= 0 {
		writeError(w, http.StatusBadRequest, "invalid library_cache_id")
		return
	}

	// A human pick is not a parse, so the store records it as "manual".
	// req.Confidence only says how well the pick scored during identify; it must
	// not claim a parse happened.
	if err := s.store.SetImportCandidateLibraryMatch(ctx, id, req.LibraryCacheID); err != nil {
		writeMatchError(w, err)
		return
	}

	row, found, err := s.store.ImportCandidateSummaryByID(ctx, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read the queue item")
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "queue item not found")
		return
	}
	patterns := s.donorPatterns(ctx)
	donors, donorErr := pairDonors(row.TargetPath, patterns)
	state, _ := deriveMatchState(row, donors, donorErr, patterns)
	writeJSON(w, http.StatusOK, matchResponse{MatchState: state})
}

// writeMatchError maps the match store sentinels onto the route's statuses. The
// 422 for a non-pending row is deliberate: the request is well-formed and
// permitted, it just cannot apply in the row's current state.
func writeMatchError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrImportCandidateNotFound), errors.Is(err, store.ErrLibraryCacheNotFound):
		writeError(w, http.StatusNotFound, "library target not found")
	case errors.Is(err, store.ErrImportCandidateMatchMismatch):
		writeAPIError(w, http.StatusConflict, "queue item is matched to a different library target", "match_conflict", "")
	case errors.Is(err, store.ErrImportCandidateKindMismatch):
		writeAPIError(w, http.StatusConflict, "the library target serves a different media kind", "kind_mismatch", "")
	case errors.Is(err, store.ErrImportCandidateInstanceMismatch):
		writeAPIError(w, http.StatusConflict, "the library target belongs to a different arr instance", "instance_mismatch", "")
	case errors.Is(err, store.ErrImportCandidateState):
		writeAPIError(w, http.StatusUnprocessableEntity, "queue item is no longer pending approval", "not_pending", "")
	default:
		writeError(w, http.StatusInternalServerError, "could not update the queue item")
	}
}

// parseQueueID reads and validates the {id} path value.
func parseQueueID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid queue id")
		return 0, false
	}
	return id, true
}

// decodeIdentifyBody tolerates an absent/empty body and rejects malformed JSON,
// so a typo never silently parses the filename instead of the operator's title.
func decodeIdentifyBody(r *http.Request) (identifyRequest, error) {
	var req identifyRequest
	if r.Body == nil {
		return req, nil
	}
	err := json.NewDecoder(r.Body).Decode(&req)
	if errors.Is(err, io.EOF) {
		return req, nil
	}
	return req, err
}

// decodeMatchBody decodes the required match body. An absent body decodes to the
// zero value, which the handler rejects as a missing library_cache_id.
func decodeMatchBody(r *http.Request) (matchRequest, error) {
	var req matchRequest
	if r.Body == nil {
		return req, nil
	}
	err := json.NewDecoder(r.Body).Decode(&req)
	if errors.Is(err, io.EOF) {
		return req, nil
	}
	return req, err
}

// identifyKind resolves the arr kind for a queue row: the kind its matched
// library row proves, the instance it was linked to, or — when the appliance
// has exactly one enabled connection — that instance. An unmatched row with two
// configured instances has no provable kind, and the route answers 409 instead
// of guessing one.
func (s *Service) identifyKind(ctx context.Context, row store.IdentifyCandidate) string {
	if kind := arrKindForMediaType(row.MediaType); kind != "" {
		return kind
	}
	switch arr.Kind(row.Kind) {
	case arr.KindRadarr, arr.KindSonarr:
		return row.Kind
	}
	conns, err := arr.ConfiguredConnections(ctx, s.settings)
	if err != nil {
		return ""
	}
	kinds := make([]string, 0, 1)
	for _, conn := range conns {
		if conn.Enabled {
			kinds = append(kinds, conn.Kind)
		}
	}
	if len(kinds) == 1 {
		return kinds[0]
	}
	return ""
}

// identifyMediaType is the media kind the candidate pool is limited to, empty
// meaning both.
func (s *Service) identifyMediaType(row store.IdentifyCandidate, kind string) string {
	if row.MediaType != "" {
		return row.MediaType
	}
	return mediaTypeForKind(kind)
}

// identifyParser builds the arr client for one kind from the instance the sync
// pass already stored. A non-empty reason means the row cannot be identified as
// configured (the caller answers 409 not_configured); a returned error is a
// store fault. The decrypted key is only ever handed to the arr client.
func (s *Service) identifyParser(ctx context.Context, kind string) (parser, string, error) {
	if s.secrets == nil {
		return nil, "no arr secret store is configured", nil
	}
	instance, ok, err := s.store.ArrInstanceByKind(ctx, kind)
	if err != nil {
		return nil, "", err
	}
	if !ok {
		return nil, "the " + kind + " instance is not configured", nil
	}
	if !instance.Enabled {
		return nil, "the " + kind + " instance is disabled", nil
	}
	if len(instance.APIKeyEnc) == 0 {
		return nil, "the " + kind + " instance has no API key", nil
	}
	apiKey, err := s.secrets.DecryptString(instance.APIKeyEnc)
	if err != nil {
		return nil, "the " + kind + " API key could not be read", nil
	}
	switch kind {
	case string(arr.KindRadarr):
		client, err := arr.NewRadarrClient(instance.BaseURL, apiKey, arr.DefaultTimeout)
		if err != nil {
			return nil, "the " + kind + " connection is not usable", nil
		}
		return client, "", nil
	case string(arr.KindSonarr):
		client, err := arr.NewSonarrClient(instance.BaseURL, apiKey, arr.DefaultTimeout)
		if err != nil {
			return nil, "the " + kind + " connection is not usable", nil
		}
		return client, "", nil
	default:
		return nil, "the " + kind + " instance is not an arr this appliance serves", nil
	}
}

// identifyDeadline is the parse budget: the configured timeout when one was
// wired (tests need a short one), else identifyTimeout.
func (s *Service) identifyDeadline() time.Duration {
	if s.identifyTimeout > 0 {
		return s.identifyTimeout
	}
	return identifyTimeout
}

// classifyIdentifyError turns a client failure into the operator-facing pair.
// The arr clients already scrub the API key from their errors, and the messages
// here name no credential.
func classifyIdentifyError(err error) (class, message string) {
	var status *arr.StatusError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout", "the instance did not answer the parse request in time"
	case errors.Is(err, arr.ErrAuth):
		return "auth", "the instance rejected the API key"
	case errors.Is(err, arr.ErrNetwork):
		return "network", "could not reach the instance: check the base URL and network"
	case errors.As(err, &status):
		return "status", fmt.Sprintf("the instance refused the parse request (HTTP %d)", status.Code)
	default:
		return "unknown", "the instance did not parse the release name"
	}
}

// arrKindForMediaType maps an arr_library_cache media type to its arr kind.
func arrKindForMediaType(mediaType string) string {
	switch mediaType {
	case "movie":
		return string(arr.KindRadarr)
	case "episode":
		return string(arr.KindSonarr)
	default:
		return ""
	}
}

// mediaTypeForKind is the inverse of arrKindForMediaType.
func mediaTypeForKind(kind string) string {
	switch arr.Kind(kind) {
	case arr.KindRadarr:
		return "movie"
	case arr.KindSonarr:
		return "episode"
	default:
		return ""
	}
}

// rankCandidates scores the cache pool against the parse verdict, drops rows
// that do not match at all, sorts best-first and trims the list.
//
// The score is deterministic and monotonic in how much of the identity agrees:
// an exact title with the parsed year is 1, an exact title alone 0.8, a
// candidate title contained in the parsed one 0.6, and the reverse 0.5. Ties
// break on title then id so the same cache always yields the same order.
func rankCandidates(parsedTitle string, parsedYear int, pool []store.LibraryCandidate) []identifyCandidate {
	out := []identifyCandidate{}
	for _, item := range pool {
		score := scoreCandidate(parsedTitle, parsedYear, item)
		if score <= 0 {
			continue
		}
		out = append(out, identifyCandidate{
			LibraryCacheID: item.ID,
			Title:          item.Title,
			Year:           item.Year,
			MediaType:      item.MediaType,
			ExternalItemID: item.ExternalItemID,
			Confidence:     score,
			MatchedBy:      "parse",
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Confidence != out[j].Confidence {
			return out[i].Confidence > out[j].Confidence
		}
		left, right := strings.ToLower(out[i].Title), strings.ToLower(out[j].Title)
		if left != right {
			return left < right
		}
		return out[i].LibraryCacheID < out[j].LibraryCacheID
	})
	if len(out) > identifyCandidateLimit {
		out = out[:identifyCandidateLimit]
	}
	return out
}

// scoreCandidate is the deterministic 0..1 confidence for one cache row. A
// candidate that shares nothing with the parse never reaches the operator: a
// LIKE hit on the pool term alone is not evidence of identity.
func scoreCandidate(parsedTitle string, parsedYear int, item store.LibraryCandidate) float64 {
	parsed := normaliseTitle(parsedTitle)
	candidate := normaliseTitle(item.Title)
	if parsed == "" || candidate == "" {
		return 0
	}
	switch {
	case parsed == candidate && parsedYear > 0 && item.Year == parsedYear:
		return 1
	case parsed == candidate:
		return 0.8
	case strings.Contains(parsed, candidate):
		return 0.6
	case strings.Contains(candidate, parsed):
		return 0.5
	default:
		return 0
	}
}

// normaliseTitle lowercases and collapses whitespace so punctuation-free release
// titles compare with cache titles. It deliberately does not strip punctuation:
// "Kraina Lodu: The Series" must stay distinct from "Kraina Lodu".
func normaliseTitle(title string) string {
	return strings.ToLower(strings.Join(strings.Fields(title), " "))
}

// poolTerm narrows the candidate pool to one word of the parsed title, so the
// LIKE search finds both longer and shorter cache titles. A parse that produced
// no usable word yields "", which the store reads as "every row of this kind".
func poolTerm(parsedTitle string) string {
	fields := strings.Fields(parsedTitle)
	for _, field := range fields {
		cleaned := strings.Trim(field, `.,:;!?()[]{}'"-_`)
		if len([]rune(cleaned)) >= 3 {
			return cleaned
		}
	}
	if len(fields) == 1 {
		return strings.Trim(fields[0], `.,:;!?()[]{}'"-_`)
	}
	return ""
}
