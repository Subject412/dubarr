package arr_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Subject412/dubarr/backend/internal/arr"
	"github.com/Subject412/dubarr/backend/internal/arr/arrmock"
)

// v5EpisodeFixtures is one series' episode with a file on disk. The file is
// present in the fixture, so a fake that embeds it unconditionally — or a
// client that forgets to ask for it — cannot both look right.
func v5EpisodeFixtures() arrmock.Fixtures {
	return arrmock.Fixtures{
		Series: []arrmock.Series{
			{ID: 1, Title: "The Expanse", Year: 2015, TvdbID: 280619, Path: "/media/tv/The Expanse"},
		},
		Episodes: []arrmock.Episode{
			{
				ID: 42, SeriesID: 1, SeasonNumber: 1, EpisodeNumber: 1,
				Title: "Dulcinea", HasFile: true,
				EpisodeFile: &arrmock.EpisodeFile{
					ID: 7, SeriesID: 1, SeasonNumber: 1,
					RelativePath: "Season 01/S01E01.mkv",
					Path:         "/media/tv/The Expanse/Season 01/S01E01.mkv",
				},
			},
		},
	}
}

// TestSonarrClientUsesV5Paths pins the exact routes a Sonarr client sends:
// v5 for every request, and the episode list carrying the file sub-resource.
// It fails as soon as any call falls back to /api/v3.
func TestSonarrClientUsesV5Paths(t *testing.T) {
	ctx := context.Background()
	srv := arrmock.New(arrmock.Sonarr, "", v5EpisodeFixtures())
	defer srv.Close()

	c, err := arr.NewSonarrClient(srv.URL, srv.APIKey(), 2*time.Second)
	if err != nil {
		t.Fatalf("NewSonarrClient: %v", err)
	}
	if err := c.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if _, err := c.Series(ctx); err != nil {
		t.Fatalf("Series: %v", err)
	}
	if _, err := c.Episodes(ctx, 1); err != nil {
		t.Fatalf("Episodes: %v", err)
	}

	want := []string{
		"/api/v5/system/status",
		"/api/v5/series",
		"/api/v5/episode?seriesId=1&includeSubresources=episodeFile",
	}
	reqs := srv.Requests()
	got := make([]string, 0, len(reqs))
	for _, r := range reqs {
		got = append(got, r.URL())
		if strings.Contains(r.URL(), "/api/v3") {
			t.Errorf("request %s asked the Sonarr v3 API", r.URL())
		}
	}
	if len(got) != len(want) {
		t.Fatalf("paths = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("path[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestSonarrEpisodesAskForTheEpisodeFile is the bug the unit exists for:
// hasFile is not enough, Sonarr only embeds episodeFile when the caller opts in,
// so the parsed episode must carry the file path.
func TestSonarrEpisodesAskForTheEpisodeFile(t *testing.T) {
	ctx := context.Background()
	srv := arrmock.New(arrmock.Sonarr, "", v5EpisodeFixtures())
	defer srv.Close()

	c, err := arr.NewSonarrClient(srv.URL, srv.APIKey(), 2*time.Second)
	if err != nil {
		t.Fatalf("NewSonarrClient: %v", err)
	}
	episodes, err := c.Episodes(ctx, 1)
	if err != nil {
		t.Fatalf("Episodes: %v", err)
	}
	if len(episodes) != 1 {
		t.Fatalf("got %d episodes, want 1", len(episodes))
	}
	ep := episodes[0]
	if !ep.HasFile {
		t.Fatalf("episode = %+v, want hasFile", ep)
	}
	if ep.EpisodeFile == nil {
		t.Fatalf("episode has no file: the request did not ask for the episodeFile sub-resource")
	}
	if want := "/media/tv/The Expanse/Season 01/S01E01.mkv"; ep.EpisodeFile.Path != want {
		t.Errorf("episode file path = %q, want %q", ep.EpisodeFile.Path, want)
	}

	// The ask itself is the contract, so assert the parameter on the wire.
	var asked bool
	for _, r := range srv.Requests() {
		if r.Path != "/api/v5/episode" {
			continue
		}
		if !strings.Contains(r.Query, "includeSubresources=episodeFile") {
			t.Errorf("episode request query = %q, want includeSubresources=episodeFile", r.Query)
		}
		asked = true
	}
	if !asked {
		t.Fatalf("no GET /api/v5/episode reached the instance: %+v", srv.Requests())
	}
}

// TestSonarrMissingV5IsALoudError proves an instance that does not serve v5 is
// reported as such — naming the product, the version expected and the path
// tried — instead of a silent fallback or a bare 404.
func TestSonarrMissingV5IsALoudError(t *testing.T) {
	// An instance that serves no *arr API at all: every path is a 404.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
	}))
	defer srv.Close()

	const apiKey = "loud-404-do-not-leak"
	c, err := arr.NewSonarrClient(srv.URL, apiKey, time.Second)
	if err != nil {
		t.Fatalf("NewSonarrClient: %v", err)
	}
	err = c.Ping(context.Background())
	if err == nil {
		t.Fatal("Ping succeeded against an instance that serves no API version")
	}

	var versionErr *arr.APIVersionError
	if !errors.As(err, &versionErr) {
		t.Fatalf("Ping error = %v, want an *arr.APIVersionError", err)
	}
	if versionErr.Kind != arr.KindSonarr || versionErr.APIBase != "/api/v5" || versionErr.Code != http.StatusNotFound {
		t.Errorf("APIVersionError = %+v, want sonarr /api/v5 404", versionErr)
	}
	msg := err.Error()
	for _, want := range []string{"sonarr", "/api/v5", "Sonarr 5"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not name %q", msg, want)
		}
	}
	if strings.Contains(msg, "/api/v3") {
		t.Errorf("error %q still names the v3 API", msg)
	}
	if strings.Contains(msg, apiKey) {
		t.Errorf("error %q echoes the API key", msg)
	}
	// The generic class still applies, so callers that only know the
	// ErrUnexpectedStatus taxonomy keep working.
	if !errors.Is(err, arr.ErrUnexpectedStatus) {
		t.Errorf("error %v does not classify as ErrUnexpectedStatus", err)
	}

	// The library list is the first call of a poll pass, so it must fail with
	// the same loud text: one specific line per pass, not a 404 per series.
	if _, err := c.Series(context.Background()); !errors.As(err, &versionErr) {
		t.Errorf("Series error = %v, want the same *arr.APIVersionError as Ping", err)
	} else if !strings.Contains(err.Error(), "Sonarr 5") {
		t.Errorf("Series error %q does not name the required Sonarr version", err)
	}
}

// TestRadarrStaysOnV3 is the other half of the decision: Radarr ships v3 only,
// so no request may be sent to /api/v5.
func TestRadarrStaysOnV3(t *testing.T) {
	ctx := context.Background()
	srv := arrmock.New(arrmock.Radarr, "", radarrFixtures())
	defer srv.Close()

	c, err := arr.NewRadarrClient(srv.URL, srv.APIKey(), 2*time.Second)
	if err != nil {
		t.Fatalf("NewRadarrClient: %v", err)
	}
	if err := c.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if _, err := c.Movies(ctx); err != nil {
		t.Fatalf("Movies: %v", err)
	}
	if _, err := c.MovieByID(ctx, 7); err != nil {
		t.Fatalf("MovieByID: %v", err)
	}
	if _, err := c.Tags(ctx); err != nil {
		t.Fatalf("Tags: %v", err)
	}

	reqs := srv.Requests()
	if len(reqs) == 0 {
		t.Fatal("the Radarr fake saw no requests")
	}
	for _, r := range reqs {
		if strings.Contains(r.URL(), "/api/v5") {
			t.Errorf("request %s asked a Radarr for the v5 API", r.URL())
		}
		if !strings.HasPrefix(r.Path, "/api/v3") {
			t.Errorf("request %s is not on the Radarr v3 API", r.URL())
		}
	}
}

// TestArrMockEpisodeSubresourceContract pins the fake itself, because a fake
// that always embeds episodeFile cannot fail the way the real instance does.
func TestArrMockEpisodeSubresourceContract(t *testing.T) {
	srv := arrmock.New(arrmock.Sonarr, "", v5EpisodeFixtures())
	defer srv.Close()

	get := func(t *testing.T, path string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		req.Header.Set("X-Api-Key", srv.APIKey())
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read GET %s body: %v", path, err)
		}
		return resp.StatusCode, string(body)
	}

	// firstEpisode decodes the single served episode from an episode-list body.
	firstEpisode := func(t *testing.T, body string) map[string]any {
		t.Helper()
		var episodes []map[string]any
		if err := json.Unmarshal([]byte(body), &episodes); err != nil {
			t.Fatalf("episode body is not a JSON array: %v (%s)", err, body)
		}
		if len(episodes) != 1 {
			t.Fatalf("got %d episodes, want 1: %s", len(episodes), body)
		}
		return episodes[0]
	}

	t.Run("no subresource means no episodeFile", func(t *testing.T) {
		code, body := get(t, "/api/v5/episode?seriesId=1")
		if code != http.StatusOK {
			t.Fatalf("GET episodes = %d: %s", code, body)
		}
		ep := firstEpisode(t, body)
		if _, ok := ep["episodeFile"]; ok {
			t.Errorf("episodeFile present without includeSubresources: %s", body)
		}
		if hasFile, _ := ep["hasFile"].(bool); !hasFile {
			t.Errorf("hasFile = %v, want true: the trap is hasFile without episodeFile", ep["hasFile"])
		}
	})

	for _, tc := range []struct {
		name  string
		query string
	}{
		{name: "v5 spelling", query: "includeSubresources=EpisodeFile"},
		{name: "lower case", query: "includeSubresources=episodefile"},
		{name: "comma separated", query: "includeSubresources=series,EpisodeFile"},
		{name: "repeated parameter", query: "includeSubresources=series&includeSubresources=episodeFile"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, body := get(t, "/api/v5/episode?seriesId=1&"+tc.query)
			if code != http.StatusOK {
				t.Fatalf("GET episodes = %d: %s", code, body)
			}
			ep := firstEpisode(t, body)
			file, ok := ep["episodeFile"].(map[string]any)
			if !ok {
				t.Fatalf("episodeFile missing for %s: %s", tc.query, body)
			}
			if want := "/media/tv/The Expanse/Season 01/S01E01.mkv"; file["path"] != want {
				t.Errorf("episodeFile.path = %v, want %s", file["path"], want)
			}
		})
	}

	t.Run("the other product's version is not served", func(t *testing.T) {
		code, body := get(t, "/api/v3/episode?seriesId=1")
		if code != http.StatusNotFound {
			t.Fatalf("GET /api/v3/episode = %d, want 404: %s", code, body)
		}
	})

	t.Run("radarr does not serve v5", func(t *testing.T) {
		radarr := arrmock.New(arrmock.Radarr, "", arrmock.Fixtures{})
		defer radarr.Close()
		req, err := http.NewRequest(http.MethodGet, radarr.URL+"/api/v5/system/status", nil)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		req.Header.Set("X-Api-Key", radarr.APIKey())
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET /api/v5/system/status: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("GET /api/v5/system/status on a Radarr fake = %d, want 404", resp.StatusCode)
		}
	})
}
