// Package naming renders Dubarr's arr-style vault name patterns.
//
// Dubarr names audio tracks, not releases: a template produces the *stem* of a
// vault file and never its extension. Every archived file ends in
// ".<iso2>.mka" (schema.sql, audio_tracks.file_path, and vault.Writer at
// publish time), so [VaultPath] is the only function here that appends the
// suffix and {Language} is deliberately not a token: a token that could
// contradict the suffix would be a foot-gun, not a feature.
//
// Grammar: literal text plus {Token} placeholders. Token names are matched
// case-insensitively and render in canonical form:
//
//	{Title} {Year} {Season:00} {Episode:00} {Episode Title}
//	{Variant} {Codec} {Channels} {TmdbId} {TvdbId} {ImdbId}
//
// The ID tokens carry the ids the arr mirror already stores, rendered bare
// ("129", "tt0245429") so the template supplies the decoration: a folder name
// and a file name can then share one spelling. An id the mirror does not know
// renders empty and drops its group, never "0".
//
// Only {Season} and {Episode} take a modifier, and only zero padding (":00" is
// two digits, ":000" three). Anything else — an unknown token, a rejected token
// ({Language}, {Lang}, {ISO2}, {Ext}, {Extension}, {Original Title}), a stray
// brace, a modifier on a token that has no number — is a validation error the
// settings layer refuses at save time.
//
// Rendering order is fixed so every caller agrees: substitute (empty string for
// an unknown value, never "0"/"nil"), drop the separators an empty token
// orphaned, remove empty optional groups ((), [], {}) until stable, tidy the
// space an empty token left just inside a surviving group ("[ 5.1]" ->
// "[5.1]"), collapse spaces and dangling dashes, clean, then reject an
// empty/reserved/over-long stem or one that already ends in .mka. [Parse] also
// refuses at save time a pattern that is statically over the byte cap or that
// ends in ".mka" itself.
//
// The cleaning rules are adapted from Sonarr's
// NzbDrone.Core/Organizer/{FileNameBuilder,FileNameValidationService} and
// Radarr's Organizer/FileNameBuilder — the behaviours, not the classes: Linux is
// the only supported platform (ADR-0016), but a vault is routinely shared over
// SMB, so Windows-reserved names are still refused.
package naming

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

const (
	// MaxStemBytes caps one rendered file name. Over-length is a rendered-plan
	// error, never a silent truncation: a truncated name can silently collide
	// with a different track.
	MaxStemBytes = 200

	// MaxPathBytes caps the whole destination path, for the same reason and
	// with room for an SMB share's own prefix.
	MaxPathBytes = 3800

	// VaultSuffix is the container extension every vault file carries.
	VaultSuffix = ".mka"
)

// Track is everything a template may read. Empty strings and non-positive
// numbers render as the empty string, never as "0"/"nil": an unmatched episode
// title (ADR-0014 forbids inventing metadata) must leave a clean name behind,
// not the literal text "0".
type Track struct {
	Title        string
	EpisodeTitle string
	Variant      string
	Codec        string
	Channels     string
	LangISO2     string
	Year         int
	Season       int
	Episode      int
	// TMDBID, TVDBID and IMDBID are the ids the arr mirror stores
	// (vault_media.tmdb_id/tvdb_id, arr_library_cache.imdb_id). A non-positive
	// number or an empty string is "the mirror does not know it": the token
	// renders empty and never "0".
	TMDBID int
	TVDBID int
	IMDBID string
	// MediaFolderPath is the folder Dubarr already owns (vault_media.
	// vault_folder_path). Directory choice is never re-derived from a
	// template, so this is only carried for [VaultPath].
	MediaFolderPath string
}

type token int

const (
	tokenLiteral token = iota
	tokenTitle
	tokenYear
	tokenSeason
	tokenEpisode
	tokenEpisodeTitle
	tokenVariant
	tokenCodec
	tokenChannels
	tokenTMDB
	tokenTVDB
	tokenIMDB
)

// canonical token names, used by the error text and the UI's token list.
var tokenNames = map[token]string{
	tokenTitle:        "Title",
	tokenYear:         "Year",
	tokenSeason:       "Season",
	tokenEpisode:      "Episode",
	tokenEpisodeTitle: "Episode Title",
	tokenVariant:      "Variant",
	tokenCodec:        "Codec",
	tokenChannels:     "Channels",
	tokenTMDB:         "TmdbId",
	tokenTVDB:         "TvdbId",
	tokenIMDB:         "ImdbId",
}

// TokenList is the documented token set, in the order the settings UI lists it.
const TokenList = "{Title}, {Year}, {Season:00}, {Episode:00}, {Episode Title}, {Variant}, {Codec}, {Channels}, {TmdbId}, {TvdbId}, {ImdbId}"

var tokenByName = map[string]token{
	"title":         tokenTitle,
	"year":          tokenYear,
	"season":        tokenSeason,
	"episode":       tokenEpisode,
	"episode title": tokenEpisodeTitle,
	"variant":       tokenVariant,
	"codec":         tokenCodec,
	"channels":      tokenChannels,
	"tmdbid":        tokenTMDB,
	"tvdbid":        tokenTVDB,
	"imdbid":        tokenIMDB,
}

// rejectedTokens are named individually because they are the tokens an arr user
// reaches for first. Refusing them with a reason is friendlier than "unknown".
var rejectedTokens = map[string]struct{}{
	"language":       {},
	"lang":           {},
	"iso2":           {},
	"ext":            {},
	"extension":      {},
	"original title": {},
}

// part is one literal run or one token placeholder.
type part struct {
	lit string
	tok token
	pad int
}

// Template is a parsed pattern. The zero value renders nothing and is rejected
// by Render.
type Template struct {
	raw   string
	parts []part
}

// Raw returns the pattern as written, for audit detail and error messages.
func (t Template) Raw() string { return t.raw }

// Parse turns a pattern into a Template, enforcing the token whitelist, the
// modifier rules and the static rules a pattern can never satisfy (a trailing
// ".mka", a minimum rendered length over [MaxStemBytes]). It performs no
// rendering, so a template that parses may still be refused by [Validate] or
// [Template.Render] for what its values produce.
func Parse(raw string) (Template, error) {
	tmpl := Template{raw: raw}
	var lit strings.Builder
	flush := func() {
		if lit.Len() > 0 {
			tmpl.parts = append(tmpl.parts, part{lit: lit.String()})
			lit.Reset()
		}
	}

	for i := 0; i < len(raw); {
		switch raw[i] {
		case '{':
			end := strings.IndexByte(raw[i+1:], '}')
			if end < 0 {
				return Template{}, fmt.Errorf("unclosed '{' in %q", raw)
			}
			flush()
			p, err := parseToken(raw[i+1 : i+1+end])
			if err != nil {
				return Template{}, err
			}
			tmpl.parts = append(tmpl.parts, p)
			i += end + 2
		case '}':
			return Template{}, fmt.Errorf("unmatched '}' in %q", raw)
		default:
			lit.WriteByte(raw[i])
			i++
		}
	}
	flush()
	if err := tmpl.checkStatic(); err != nil {
		return Template{}, err
	}
	return tmpl, nil
}

// checkStatic refuses a pattern that cannot render a legal stem for *any*
// track, so a hopeless pattern fails at save time instead of on the first
// file: the literal bytes plus one byte per token (the shortest a value can
// render) must fit [MaxStemBytes], and the pattern must not already end in
// ".mka" because [VaultPath] appends ".<iso2>.mka" ("….mka.pl.mka").
//
// A pattern that only grows past the cap once real values are substituted is a
// per-file error in [cleanStem], not a pattern error.
func (t Template) checkStatic() error {
	if len(t.parts) == 0 {
		return nil // a blank pattern renders nothing; Validate refuses it.
	}
	minBytes := 0
	for _, p := range t.parts {
		if p.tok == tokenLiteral {
			minBytes += len(p.lit)
			continue
		}
		minBytes++
	}
	if minBytes > MaxStemBytes {
		return fmt.Errorf("the pattern renders at least %d bytes; the limit is %d", minBytes, MaxStemBytes)
	}
	last := t.parts[len(t.parts)-1]
	if last.tok == tokenLiteral && strings.EqualFold(filepath.Ext(strings.TrimSpace(last.lit)), VaultSuffix) {
		return fmt.Errorf("the pattern must not end in %s: the vault appends .<language>%s", VaultSuffix, VaultSuffix)
	}
	return nil
}

func parseToken(body string) (part, error) {
	name, modifier := body, ""
	if idx := strings.IndexByte(body, ':'); idx >= 0 {
		name, modifier = body[:idx], body[idx+1:]
	}
	written := strings.TrimSpace(name)
	key := tokenKey(name)

	if _, rejected := rejectedTokens[key]; rejected {
		return part{}, fmt.Errorf("`{%s}` is not available: vault files always end in `.<language>%s`.", written, VaultSuffix)
	}
	tok, known := tokenByName[key]
	if !known {
		return part{}, fmt.Errorf("unknown token {%s}; available tokens are %s", written, TokenList)
	}
	if tok != tokenSeason && tok != tokenEpisode {
		if modifier != "" {
			return part{}, fmt.Errorf("modifier :%s is not supported on {%s}; only {Season:00} and {Episode:00} take zero padding",
				modifier, tokenNames[tok])
		}
		return part{tok: tok}, nil
	}
	if modifier == "" {
		return part{tok: tok}, nil
	}
	if !isZeroPad(modifier) {
		return part{}, fmt.Errorf("modifier :%s is not supported on {%s}; only zero padding like :00 or :000",
			modifier, tokenNames[tok])
	}
	return part{tok: tok, pad: len(modifier)}, nil
}

// tokenKey normalises a written token name: case-insensitive, internal runs of
// whitespace collapsed, so "{Episode  Title}" is the same token as
// "{Episode Title}".
func tokenKey(name string) string {
	return strings.ToLower(strings.Join(strings.Fields(name), " "))
}

func isZeroPad(modifier string) bool {
	if modifier == "" {
		return false
	}
	for i := 0; i < len(modifier); i++ {
		if modifier[i] != '0' {
			return false
		}
	}
	return true
}

// Render produces the stem for tr. It is the only entry point a caller needs:
// Parse already refused an unknown token, and Render refuses whatever the
// resulting name cannot be.
func (t Template) Render(tr Track) (string, error) {
	return t.render(tr, false)
}

// RenderFolder renders a template as a single directory name (the season
// folder). A blank pattern means "no subfolder" and renders as "".
func (t Template) RenderFolder(tr Track) (string, error) {
	if strings.TrimSpace(t.raw) == "" {
		return "", nil
	}
	return t.render(tr, true)
}

func (t Template) render(tr Track, folder bool) (string, error) {
	pieces := make([]piece, 0, len(t.parts))
	for _, p := range t.parts {
		if p.tok == tokenLiteral {
			pieces = append(pieces, piece{text: p.lit})
			continue
		}
		value := p.tok.value(tr, p.pad)
		pieces = append(pieces, piece{text: value, token: true, empty: value == ""})
	}

	// An empty token takes the separator run *before* it with it, which is the
	// arr behaviour: "... - {Episode Title} [{Variant} ...]" must lose the
	// " - " rather than leave a dangling dash when the episode title is
	// unknown, while the space that introduces "[..." stays, so the documented
	// result is "The Office - S02E03 [Lektor EAC3 2.0]". An optional group the
	// empty token leaves behind ("()", "[]") goes in the collapse step below.
	for i := range pieces {
		if !pieces[i].empty {
			continue
		}
		if i > 0 && !pieces[i-1].token {
			pieces[i-1].text = strings.TrimRight(pieces[i-1].text, separatorChars)
		}
	}

	var b strings.Builder
	for _, p := range pieces {
		if p.empty {
			continue
		}
		b.WriteString(p.text)
	}
	rendered := collapse(b.String())

	if folder {
		name, err := CleanFileName(rendered)
		if err != nil {
			return "", fmt.Errorf("folder name: %w", err)
		}
		return name, nil
	}
	return cleanStem(rendered)
}

type piece struct {
	text  string
	token bool
	empty bool
}

// separatorChars are the characters a template uses as decoration around a
// token. They are dropped next to an empty token and trimmed from a stem's ends.
const separatorChars = " -._"

func (tok token) value(tr Track, pad int) string {
	switch tok {
	case tokenTitle:
		return CleanTitle(tr.Title)
	case tokenYear:
		if tr.Year <= 0 {
			return ""
		}
		return strconv.Itoa(tr.Year)
	case tokenSeason:
		return padNumber(tr.Season, pad, "season")
	case tokenEpisode:
		return padNumber(tr.Episode, pad, "episode")
	case tokenEpisodeTitle:
		return CleanTitle(tr.EpisodeTitle)
	case tokenVariant:
		return variantLabel(tr.Variant)
	case tokenCodec:
		return CleanTitle(strings.ToUpper(strings.TrimSpace(tr.Codec)))
	case tokenChannels:
		return CleanTitle(strings.TrimSpace(tr.Channels))
	case tokenTMDB:
		return idValue(tr.TMDBID)
	case tokenTVDB:
		return idValue(tr.TVDBID)
	case tokenIMDB:
		// Rendered verbatim: the mirror stores IMDb's own "tt…" spelling and a
		// token must not add or strip a prefix. The store refuses an id with a
		// path separator (arrsync), and the rendered stem is cleaned anyway.
		return strings.TrimSpace(tr.IMDBID)
	}
	return ""
}

// idValue renders a numeric id bare, and the empty string when the mirror does
// not know it. The template owns the decoration ("[{TmdbId}]"), so the token
// works the same in a folder name and in a file name.
func idValue(n int) string {
	if n <= 0 {
		return ""
	}
	return strconv.Itoa(n)
}

// padNumber renders a season/episode number. A non-positive number is unknown:
// movies have no season, and rendering "00" there would invent metadata. (The
// consequence — season 0 "specials" render empty — is documented in the plan's
// underdetermined list: the token struct carries ints, not optionals.)
func padNumber(n, pad int, _ string) string {
	if n <= 0 {
		return ""
	}
	if pad <= 0 {
		return strconv.Itoa(n)
	}
	return fmt.Sprintf("%0*d", pad, n)
}

// variantLabel is the display form of audio_tracks.variant. The column's CHECK
// constraint makes the list closed, so an unexpected value is passed through
// rather than guessed at.
func variantLabel(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "dubbing":
		return "Dubbing"
	case "lektor":
		return "Lektor"
	case "original":
		return "Original"
	case "standard":
		return "Standard"
	case "commentary":
		return "Commentary"
	}
	return CleanTitle(strings.TrimSpace(v))
}

var (
	reSpaces       = regexp.MustCompile(`[[:space:]]+`)
	reEmptyParen   = regexp.MustCompile(`\([[:space:]]*\)`)
	reEmptyBracket = regexp.MustCompile(`\[[[:space:]]*\]`)
	reEmptyBrace   = regexp.MustCompile(`\{[[:space:]]*\}`)
	reOpenGap      = regexp.MustCompile(`([(\[{])[[:space:]]+`)
	reCloseGap     = regexp.MustCompile(`[[:space:]]+([)\]\}])`)
	reDashRun      = regexp.MustCompile(`(?:[[:space:]]*-[[:space:]]*){2,}`)
)

// collapse applies the documented cleanup until it stops changing the string:
// empty groups, spaces just inside a surviving group, repeated spaces, repeated
// dashes, dangling separators. Looping matters because removing one group can
// expose another ("[ () ]").
func collapse(s string) string {
	for {
		before := s
		s = reEmptyParen.ReplaceAllString(s, "")
		s = reEmptyBracket.ReplaceAllString(s, "")
		s = reEmptyBrace.ReplaceAllString(s, "")
		// An empty token inside a group leaves its neighbouring separator
		// space behind ("[ 5.1]" when only {Channels} has a value), so the
		// space next to the delimiter goes too.
		s = reOpenGap.ReplaceAllString(s, "$1")
		s = reCloseGap.ReplaceAllString(s, "$1")
		s = reSpaces.ReplaceAllString(s, " ")
		s = reDashRun.ReplaceAllString(s, " - ")
		s = strings.Trim(s, " ")
		s = strings.Trim(s, " -")
		if s == before {
			return s
		}
	}
}

// CleanTitle turns a catalog value into a name-safe component: the forbidden
// path characters and control characters become spaces, runs of spaces
// collapse, and leading/trailing dots and spaces go. It never fails; a value
// that cleans to nothing renders as an empty token.
func CleanTitle(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|', 0:
			b.WriteRune(' ')
			continue
		}
		if unicode.IsControl(r) {
			b.WriteRune(' ')
			continue
		}
		b.WriteRune(r)
	}
	out := reSpaces.ReplaceAllString(b.String(), " ")
	return strings.Trim(out, " .")
}

// CleanFileName validates a cleaned single path component. It refuses the names
// a shared vault cannot carry: an empty name, "." and "..", the Windows device
// names (SMB clients still resolve those), and anything over [MaxStemBytes].
func CleanFileName(s string) (string, error) {
	name := CleanTitle(s)
	if name == "" {
		return "", errors.New("name is empty")
	}
	if name == "." || name == ".." {
		return "", fmt.Errorf("name %q is not a valid file name", name)
	}
	if isReservedName(name) {
		return "", fmt.Errorf("name %q is reserved by Windows and cannot be shared over SMB", name)
	}
	if len(name) > MaxStemBytes {
		return "", fmt.Errorf("name is %d bytes; the limit is %d", len(name), MaxStemBytes)
	}
	return name, nil
}

// reservedNames are the DOS device names, refused in any case (and with any
// extension) because a vault folder is routinely exposed over SMB.
var reservedNames = map[string]struct{}{
	"CON": {}, "PRN": {}, "AUX": {}, "NUL": {},
	"COM1": {}, "COM2": {}, "COM3": {}, "COM4": {}, "COM5": {},
	"COM6": {}, "COM7": {}, "COM8": {}, "COM9": {},
	"LPT1": {}, "LPT2": {}, "LPT3": {}, "LPT4": {}, "LPT5": {},
	"LPT6": {}, "LPT7": {}, "LPT8": {}, "LPT9": {},
}

func isReservedName(name string) bool {
	if _, ok := reservedNames[strings.ToUpper(strings.TrimSpace(name))]; ok {
		return true
	}
	// "CON.pl.mka" is reserved on Windows too: the device name is the part
	// before the first dot.
	if i := strings.IndexByte(name, '.'); i > 0 {
		_, ok := reservedNames[strings.ToUpper(name[:i])]
		return ok
	}
	return false
}

// cleanStem applies the terminal rules to a rendered stem.
func cleanStem(rendered string) (string, error) {
	stem := CleanTitle(rendered)
	if stem == "" {
		return "", errors.New("the pattern renders an empty name")
	}
	if stem == "." || stem == ".." {
		return "", fmt.Errorf("the pattern renders %q, which is not a file name", stem)
	}
	if isReservedName(stem) {
		return "", fmt.Errorf("the pattern renders %q, a name Windows reserves", stem)
	}
	if len(stem) > MaxStemBytes {
		return "", fmt.Errorf("the rendered name is %d bytes; the limit is %d", len(stem), MaxStemBytes)
	}
	if strings.EqualFold(filepath.Ext(stem), VaultSuffix) {
		return "", fmt.Errorf("the pattern must not end in %s: the vault appends .<language>%s", VaultSuffix, VaultSuffix)
	}
	return stem, nil
}

// sampleTrack is the fixed example a preview and [Validate] render with. It
// carries every token's value so a pattern that only uses, say, {Episode Title}
// is still proven to produce something.
func sampleTrack() Track {
	return Track{
		Title:        "The Office",
		EpisodeTitle: "Office Olympics",
		Variant:      "lektor",
		Codec:        "eac3",
		Channels:     "2.0",
		LangISO2:     "pl",
		Year:         2005,
		Season:       2,
		Episode:      3,
		TMDBID:       129,
		TVDBID:       81189,
		IMDBID:       "tt0245429",
	}
}

// Sample renders the pattern with the documented example values, for the
// settings preview line. An unusable pattern renders as "".
func (t Template) Sample() string {
	stem, err := t.Render(sampleTrack())
	if err != nil {
		return ""
	}
	return stem
}

// Validate is the settings-time gate: the pattern parses, is not blank, and
// renders a legal stem for the sample track (which probes the reserved-name,
// .mka-suffix and length rules for the literal parts).
//
// It does not prove every *value* is legal — a 300-byte media title is a
// per-file error at preview time, not a reason to refuse the pattern.
func Validate(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return errors.New("the pattern must not be empty")
	}
	tmpl, err := Parse(raw)
	if err != nil {
		return err
	}
	if _, err := tmpl.Render(sampleTrack()); err != nil {
		return err
	}
	return nil
}

// VaultPath joins the media folder, the optional season folder and the suffixed
// file name. It is the only function allowed to append ".<iso2>.mka"; a unit
// test pins it against vault.Writer's own append so the two cannot drift.
//
// The caller supplies the stem from [Template.Render] and the track's ISO 639-1
// code (NOT NULL in audio_tracks). A blank season folder means "no subfolder".
func VaultPath(mediaFolder, seasonFolder string, tr Track, stem string) string {
	dir := mediaFolder
	if folder := strings.TrimSpace(seasonFolder); folder != "" {
		dir = filepath.Join(dir, folder)
	}
	return filepath.Join(dir, stem+"."+tr.LangISO2+VaultSuffix)
}
