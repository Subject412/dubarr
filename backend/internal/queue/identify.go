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

// mirrorCandidateCeiling caps a mirror hit. A LIKE hit on the release's own
// words is a substring match, not a parse, so it must never outrank a candidate
// arr's parser named.
const mirrorCandidateCeiling = 0.5

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

// identifyPoolFunc is the one store read identify searches the synced library
// with. It is a field on Service rather than a direct store call because the
// route promises a bound — at most four mirror searches per identify — and an
// injected counter is the only way a test can observe that bound. Nil means the
// real store.
type identifyPoolFunc func(ctx context.Context, instanceID int64, mediaType, term string) ([]store.LibraryCandidate, error)

// candidatePool reads the synced library through the injected lookup, falling
// back to the store itself.
func (s *Service) candidatePool(ctx context.Context, instanceID int64, mediaType, term string) ([]store.LibraryCandidate, error) {
	if s.identifyPool != nil {
		return s.identifyPool(ctx, instanceID, mediaType, term)
	}
	return s.store.IdentifyCandidatePool(ctx, instanceID, mediaType, term)
}

// identifyRequest is the optional identify body: an operator-supplied release
// name overriding the row's filename stem, and — for a release the row itself
// cannot place — the arr library to parse it in. Kind is additive: a client
// that sends only title keeps the old behaviour.
type identifyRequest struct {
	Title string `json:"title"`
	Kind  string `json:"kind"`
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
	// MatchedBy names how the candidate was found: "parse" when arr's own
	// parser named the title, "mirror" when the synced library was searched
	// on the release's words alone.
	MatchedBy string `json:"matched_by"`
}

// identifyParse is what arr thought of the release name. Parsed is false when
// the instance recognised no title: an unparsable release, not a cold cache.
// Error carries the human arr failure class when the parse call itself failed;
// a name arr simply could not read is parsed=false with no error.
type identifyParse struct {
	Parsed     bool    `json:"parsed"`
	Title      string  `json:"title"`
	Year       int     `json:"year"`
	Quality    string  `json:"quality"`
	Confidence float64 `json:"confidence"`
	Error      string  `json:"error,omitempty"`
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
	// Kinds names the arr libraries the operator must choose between. Only
	// code "ambiguous_kind" carries it, in a fixed order (radarr, sonarr) so
	// the UI can render a stable choice instead of a shuffled one.
	Kinds []string `json:"kinds,omitempty"`
}

// writeAPIError writes one error envelope.
func writeAPIError(w http.ResponseWriter, status int, message, code, class string) {
	writeJSON(w, status, apiError{Error: message, Code: code, Class: class})
}

// writeAPIKindsError writes an error envelope that also names the arr libraries
// the request has to choose between.
func writeAPIKindsError(w http.ResponseWriter, status int, message, code string, kinds []string) {
	writeJSON(w, status, apiError{Error: message, Code: code, Kinds: kinds})
}

// handleIdentify parses the release with arr and returns the ranked candidates.
//
//	200 {"parse": {...}, "candidates": [...]}
//	400 the {id} is not a positive integer, the body is malformed, code
//	    "invalid_kind" the body kind is not radarr|sonarr, or there is no
//	    release name to parse
//	404 the id names no queue row
//	409 code "kind_conflict"  the body kind contradicts the kind the release proves
//	409 code "ambiguous_kind" the release proves no kind and both libraries are
//	    enabled: kinds carries ["radarr","sonarr"] and the operator chooses
//	409 code "not_configured" no secret store, no enabled instance at all, or the
//	    chosen kind has no enabled instance or key
//	500 the arr configuration could not be read
//	502 code "identify_failed" arr refused or could not answer AND the mirror
//	    library held nothing to offer; class says why
//
// An empty library cache is not an error: the parse verdict is still worth
// showing, and candidates come back as an empty array. A release arr could not
// parse is reported as parse.parsed=false, and the synced library is searched
// anyway — an unparsable name is the case identify exists for, so an empty
// candidate array there means "nothing in your library fits", never "no search
// was run".
// An ambiguous appliance is reported as a choice, never as absence: the route
// asks which library before it parses anything, and resolves nothing from the
// filename.
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

	// The body kind is the operator's own choice, so a typo is a bad request
	// rather than something to ignore.
	requested := strings.TrimSpace(req.Kind)
	if requested != "" && !isIdentifyKind(requested) {
		writeAPIError(w, http.StatusBadRequest, "kind must be radarr or sonarr", "invalid_kind", "")
		return
	}

	kind, choices, err := s.resolveIdentifyKind(ctx, row, requested)
	if err != nil {
		if errors.Is(err, errIdentifyKindConflict) {
			writeAPIError(w, http.StatusConflict, fmt.Sprintf("the release belongs to the %s library, not %s", provenIdentifyKind(row), requested), "kind_conflict", "")
			return
		}
		// A configuration that could not be read is not an empty one: 500, so
		// the operator fixes the settings read instead of adding an instance
		// that already exists.
		writeError(w, http.StatusInternalServerError, "could not read the arr configuration")
		return
	}
	if kind == "" {
		if len(choices) > 1 {
			writeAPIKindsError(w, http.StatusConflict, "this release is unmatched: choose the arr library to identify it in ("+strings.Join(choices, " or ")+")", "ambiguous_kind", choices)
			return
		}
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
	parsed, parseErr := client.Parse(parseCtx, title)

	parseView := identifyParse{
		Parsed:     parsed.Parsed,
		Title:      parsed.Title,
		Year:       parsed.Year,
		Quality:    parsed.Quality,
		Confidence: parsed.Confidence,
	}
	var (
		parseClass   string
		parseMessage string
	)
	if parseErr != nil {
		parseClass, parseMessage = classifyIdentifyError(parseErr)
		// The verdict is unknown, not empty: parsed stays false and the class
		// message rides along, so a refused key reads differently from a name
		// arr simply could not parse.
		parseView = identifyParse{Error: parseMessage}
	}

	// The mirror is searched whether or not arr could parse the release, because
	// the unparsable names are the ones the operator needs help with. Parse wins
	// a tie: it matched the release, the mirror matched a substring.
	mediaType := s.identifyMediaType(row, kind)
	parseCandidates := []identifyCandidate{}
	if parsed.Parsed {
		pool, err := s.candidatePool(ctx, row.InstanceID, mediaType, poolTerm(parsed.Title))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "could not search the library")
			return
		}
		parseCandidates = rankCandidates(parsed.Title, parsed.Year, pool)
	}
	mirrorCandidates, err := s.rankMirrorCandidates(ctx, row.InstanceID, mediaType, mirrorTerm(title))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not search the library")
		return
	}
	candidates := mergeIdentifyCandidates(parseCandidates, mirrorCandidates)

	// An arr failure is the answer only when the mirror had nothing to offer: a
	// response that carries a usable match is never a 502, and an empty one is
	// never dressed up as success.
	if parseErr != nil && len(candidates) == 0 {
		writeAPIError(w, http.StatusBadGateway, parseMessage, "identify_failed", parseClass)
		return
	}

	writeJSON(w, http.StatusOK, identifyResponse{Parse: parseView, Candidates: candidates})
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
	state, _, _ := deriveMatchState(row, donors, donorErr, patterns)
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

// errIdentifyKindConflict reports a body kind that contradicts the kind the
// release already proves. The operator's choice is refused, never silently
// dropped: ignoring it would be the same defect as guessing one.
var errIdentifyKindConflict = errors.New("identify: the body kind contradicts the kind the release proves")

// identifyKindOrder is the only order the two libraries are ever named in. The
// choice list a 409 carries is built from this array, never from map iteration,
// so one install always shows the same order.
var identifyKindOrder = [...]string{string(arr.KindRadarr), string(arr.KindSonarr)}

// resolveIdentifyKind decides which arr library parses the release. The order is
// explicit and one-pass, strongest evidence first:
//
//  1. the media type of the library row the release is matched to — a movie row
//     and an episode row are proof, not a guess;
//  2. the kind of the arr instance the row was linked to;
//  3. the operator's body kind;
//  4. the only enabled connection, when the appliance has exactly one.
//
// A body kind that contradicts what the row proves is errIdentifyKindConflict.
// When the row proves nothing and two libraries are enabled, no kind is chosen:
// candidates carries both, in identifyKindOrder, so the caller can offer the
// choice. There is no filename heuristic here — a release that looks like a film
// is still the operator's call.
//
// No kind, no candidates means genuine absence: nothing is enabled. A settings
// read failure is returned as err so the caller can answer 500; failing to read
// is never "nothing configured".
func (s *Service) resolveIdentifyKind(ctx context.Context, row store.IdentifyCandidate, requested string) (string, []string, error) {
	proven := provenIdentifyKind(row)
	if proven != "" {
		if requested != "" && requested != proven {
			return "", nil, errIdentifyKindConflict
		}
		return proven, nil, nil
	}
	if requested != "" {
		return requested, nil, nil
	}
	conns, err := arr.ConfiguredConnections(ctx, s.settings)
	if err != nil {
		return "", nil, err
	}
	choices := make([]string, 0, len(identifyKindOrder))
	for _, kind := range identifyKindOrder {
		for _, conn := range conns {
			if conn.Enabled && conn.Kind == kind {
				choices = append(choices, kind)
				break
			}
		}
	}
	switch len(choices) {
	case 0:
		return "", nil, nil
	case 1:
		return choices[0], nil, nil
	default:
		return "", choices, nil
	}
}

// provenIdentifyKind is the kind the release itself proves: the media type of
// the library row it is matched to, or the arr kind of the instance it was
// linked to. Empty means an unmatched release that proves nothing.
func provenIdentifyKind(row store.IdentifyCandidate) string {
	if kind := arrKindForMediaType(row.MediaType); kind != "" {
		return kind
	}
	switch arr.Kind(row.Kind) {
	case arr.KindRadarr, arr.KindSonarr:
		return row.Kind
	}
	return ""
}

// isIdentifyKind reports whether kind names a library this appliance serves.
func isIdentifyKind(kind string) bool {
	switch arr.Kind(kind) {
	case arr.KindRadarr, arr.KindSonarr:
		return true
	default:
		return false
	}
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

// rankMirrorCandidates searches the synced library for the release's own words
// and ranks what it finds as a mirror hit.
//
// The ladder stops at the first rung whose LIKE found anything, so one identify
// costs at most four pool reads however unhelpful the name is. Every hit is
// capped at mirrorCandidateCeiling: it matched a substring, not the release, so
// it must never outrank a candidate the parser named.
func (s *Service) rankMirrorCandidates(ctx context.Context, instanceID int64, mediaType, term string) ([]identifyCandidate, error) {
	for _, rung := range mirrorLadder(term) {
		pool, err := s.candidatePool(ctx, instanceID, mediaType, rung)
		if err != nil {
			return nil, err
		}
		if len(pool) == 0 {
			continue
		}
		ranked := rankCandidates(term, 0, pool)
		for i := range ranked {
			ranked[i].MatchedBy = "mirror"
			if ranked[i].Confidence > mirrorCandidateCeiling {
				ranked[i].Confidence = mirrorCandidateCeiling
			}
		}
		return ranked, nil
	}
	return []identifyCandidate{}, nil
}

// mergeIdentifyCandidates puts the parse hits first, in their ranked order, then
// the mirror hits the parse did not already offer, and caps the total.
//
// The limit is the first thing the loop body checks, not the last: a check after
// the append only leaves the call that filled the list, so a parse that already
// returned the whole cap would still receive one mirror row on top of it.
func mergeIdentifyCandidates(parse, mirror []identifyCandidate) []identifyCandidate {
	out := make([]identifyCandidate, 0, len(parse)+len(mirror))
	seen := make(map[int64]struct{}, len(parse))
	appendAll := func(items []identifyCandidate) {
		for _, item := range items {
			if len(out) >= identifyCandidateLimit {
				return
			}
			if _, ok := seen[item.LibraryCacheID]; ok {
				continue
			}
			seen[item.LibraryCacheID] = struct{}{}
			out = append(out, item)
		}
	}
	appendAll(parse)
	appendAll(mirror)
	return out
}

// mirrorTerm reduces a release name to the words a synced-library title is
// likely to share with it: separators become spaces, bracketed groups go, and
// so does everything from the first four-digit year on. A trailing scene group
// marker ("-GRP") goes last.
//
// An empty result — a name that starts with its year, say — falls back to the
// first four dot-separated components rather than searching with no term at all.
func mirrorTerm(stem string) string {
	cleaned := strings.NewReplacer(".", " ", "_", " ").Replace(stem)
	cleaned = dropBracketedGroups(cleaned)
	words := strings.Fields(cleaned)
	for i, word := range words {
		if isYearToken(word) {
			words = words[:i]
			break
		}
	}
	for len(words) > 0 && strings.HasPrefix(words[len(words)-1], "-") {
		words = words[:len(words)-1]
	}
	if term := strings.Join(words, " "); term != "" {
		return term
	}
	parts := make([]string, 0, 4)
	for _, part := range strings.Split(stem, ".") {
		if part = strings.TrimSpace(part); part == "" {
			continue
		}
		parts = append(parts, part)
		if len(parts) == 4 {
			break
		}
	}
	return strings.Join(parts, " ")
}

// mirrorLadder is the query ladder for one mirror search: the whole term, then
// its first three, two and one words, deduplicated — at most four queries.
//
// The shorter rungs stop at three characters, because a one- or two-character
// LIKE would scan the whole library and offer noise. The full term is always
// tried, even a short one: a two-letter film title is still the operator's name.
func mirrorLadder(term string) []string {
	words := strings.Fields(term)
	rungs := make([]string, 0, 4)
	add := func(part []string) {
		if len(part) == 0 {
			return
		}
		candidate := strings.Join(part, " ")
		for _, existing := range rungs {
			if existing == candidate {
				return
			}
		}
		rungs = append(rungs, candidate)
	}
	add(words)
	for _, n := range []int{3, 2, 1} {
		if len(words) <= n {
			continue
		}
		if len([]rune(strings.Join(words[:n], " "))) < 3 {
			continue
		}
		add(words[:n])
	}
	return rungs
}

// dropBracketedGroups removes [...] and (...) groups from a release name,
// keeping an unbalanced closer as the literal character it is.
func dropBracketedGroups(name string) string {
	var b strings.Builder
	depth := 0
	for _, r := range name {
		switch r {
		case '[', '(':
			depth++
		case ']', ')':
			if depth > 0 {
				depth--
				continue
			}
			b.WriteRune(r)
		default:
			if depth == 0 {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

// isYearToken reports whether one release-name token is exactly a four-digit
// year, which is where the title stops and the release metadata starts.
func isYearToken(word string) bool {
	if len(word) != 4 {
		return false
	}
	for _, r := range word {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
