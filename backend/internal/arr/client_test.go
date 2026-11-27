package arr_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Subject412/dubarr/backend/internal/arr"
	"github.com/Subject412/dubarr/backend/internal/arr/arrmock"
)

func radarrFixtures() arrmock.Fixtures {
	return arrmock.Fixtures{
		Movies: []arrmock.Movie{
			{
				ID: 7, Title: "Shrek", Year: 2001, TmdbID: 808,
				Path: "/media/movies/Shrek (2001)", FolderName: "Shrek (2001)",
				QualityProfileID: 4, Monitored: true, HasFile: true,
				OriginalLanguage: arrmock.Language{ID: 1, Name: "English"},
			},
			{
				ID: 9, Title: "Minions", Year: 2015, TmdbID: 211672,
				Path: "/media/movies/Minions (2015)", FolderName: "Minions (2015)",
				QualityProfileID: 4, Monitored: false, HasFile: false,
				OriginalLanguage: arrmock.Language{ID: 1, Name: "English"},
			},
		},
		QualityProfiles: []arrmock.QualityProfile{{ID: 4, Name: "HD-1080p"}, {ID: 6, Name: "Ultra-HD"}},
		RootFolders:     []arrmock.RootFolder{{ID: 1, Path: "/media/movies", Accessible: true, FreeSpace: 1 << 40}},
	}
}

func sonarrFixtures() arrmock.Fixtures {
	return arrmock.Fixtures{
		Series: []arrmock.Series{
			{ID: 3, Title: "The Expanse", Year: 2015, TvdbID: 280619, Path: "/media/tv/The Expanse", FolderName: "The Expanse", QualityProfileID: 2, Monitored: true},
			{ID: 5, Title: "Dark", Year: 2017, TvdbID: 334824, Path: "/media/tv/Dark", FolderName: "Dark", QualityProfileID: 2, Monitored: true},
		},
		QualityProfiles: []arrmock.QualityProfile{{ID: 2, Name: "HD-720p"}},
		RootFolders:     []arrmock.RootFolder{{ID: 1, Path: "/media/tv", Accessible: true, FreeSpace: 2 << 40}},
	}
}

func TestRadarrClientReadsAgainstMock(t *testing.T) {
	ctx := context.Background()
	srv := arrmock.New(arrmock.Radarr, "", radarrFixtures())
	defer srv.Close()

	c, err := arr.NewRadarrClient(srv.URL, srv.APIKey(), 2*time.Second)
	if err != nil {
		t.Fatalf("NewRadarrClient: %v", err)
	}
	var client arr.Radarr = c
	if client.Kind() != arr.KindRadarr {
		t.Errorf("Kind() = %q, want %q", client.Kind(), arr.KindRadarr)
	}

	tests := []struct {
		name string
		call func() (any, error)
		// check receives the decoded value and must report mismatches.
		check func(t *testing.T, v any)
	}{
		{
			name:  "ping",
			call:  func() (any, error) { return nil, client.Ping(ctx) },
			check: func(t *testing.T, _ any) {},
		},
		{
			name: "movies",
			call: func() (any, error) { return client.Movies(ctx) },
			check: func(t *testing.T, v any) {
				movies := v.([]arr.Movie)
				if len(movies) != 2 {
					t.Fatalf("got %d movies, want 2", len(movies))
				}
				got := movies[0]
				want := arr.Movie{
					ID: 7, Title: "Shrek", Year: 2001, TmdbID: 808,
					Path: "/media/movies/Shrek (2001)", FolderName: "Shrek (2001)",
					QualityProfileID: 4, Monitored: true, HasFile: true,
					OriginalLanguage: arr.Language{ID: 1, Name: "English"},
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("movie[0] = %+v, want %+v", got, want)
				}
				if movies[1].Monitored || movies[1].HasFile {
					t.Errorf("movie[1] flags = %+v", movies[1])
				}
			},
		},
		{
			name: "quality profiles",
			call: func() (any, error) { return client.QualityProfiles(ctx) },
			check: func(t *testing.T, v any) {
				got := v.([]arr.QualityProfile)
				if len(got) != 2 || got[1].ID != 6 || got[1].Name != "Ultra-HD" {
					t.Errorf("profiles = %+v", got)
				}
			},
		},
		{
			name: "root folders",
			call: func() (any, error) { return client.RootFolders(ctx) },
			check: func(t *testing.T, v any) {
				got := v.([]arr.RootFolder)
				if len(got) != 1 || got[0].Path != "/media/movies" || !got[0].Accessible || got[0].FreeSpace != 1<<40 {
					t.Errorf("root folders = %+v", got)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, err := tt.call()
			if err != nil {
				t.Fatalf("%s: %v", tt.name, err)
			}
			tt.check(t, v)
		})
	}

	reqs := srv.Requests()
	if len(reqs) != 4 {
		t.Fatalf("server saw %d requests, want 4: %+v", len(reqs), reqs)
	}
	for _, r := range reqs {
		if r.Method != httpMethodGet {
			t.Errorf("%s %s: method = %s", r.Method, r.Path, r.Method)
		}
		if r.APIKey != srv.APIKey() {
			t.Errorf("%s: X-Api-Key = %q, want the configured key", r.Path, r.APIKey)
		}
	}
	if reqs[1].Path != "/api/v3/movie" {
		t.Errorf("movies path = %q", reqs[1].Path)
	}
}

func TestSonarrClientReadsAgainstMock(t *testing.T) {
	ctx := context.Background()
	srv := arrmock.New(arrmock.Sonarr, "", sonarrFixtures())
	defer srv.Close()

	c, err := arr.NewSonarrClient(srv.URL, srv.APIKey(), 2*time.Second)
	if err != nil {
		t.Fatalf("NewSonarrClient: %v", err)
	}
	var client arr.Sonarr = c
	if client.Kind() != arr.KindSonarr {
		t.Errorf("Kind() = %q, want %q", client.Kind(), arr.KindSonarr)
	}

	series, err := client.Series(ctx)
	if err != nil {
		t.Fatalf("Series: %v", err)
	}
	if len(series) != 2 || series[1].Title != "Dark" || series[1].TvdbID != 334824 {
		t.Errorf("series = %+v", series)
	}

	profiles, err := client.QualityProfiles(ctx)
	if err != nil {
		t.Fatalf("QualityProfiles: %v", err)
	}
	if len(profiles) != 1 || profiles[0].Name != "HD-720p" {
		t.Errorf("profiles = %+v", profiles)
	}

	roots, err := client.RootFolders(ctx)
	if err != nil {
		t.Fatalf("RootFolders: %v", err)
	}
	if len(roots) != 1 || roots[0].Path != "/media/tv" {
		t.Errorf("root folders = %+v", roots)
	}

	if err := client.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

func TestClientErrorModel(t *testing.T) {
	ctx := context.Background()

	t.Run("auth failure", func(t *testing.T) {
		srv := arrmock.New(arrmock.Radarr, "", arrmock.Fixtures{})
		defer srv.Close()

		c, err := arr.NewRadarrClient(srv.URL, "wrong-key", time.Second)
		if err != nil {
			t.Fatalf("NewRadarrClient: %v", err)
		}
		if _, err := c.Movies(ctx); !errors.Is(err, arr.ErrAuth) {
			t.Fatalf("Movies error = %v, want ErrAuth", err)
		}
		err = c.Ping(ctx)
		if !errors.Is(err, arr.ErrAuth) {
			t.Fatalf("Ping error = %v, want ErrAuth", err)
		}
		// The status code must be structural (not parsed out of the message)
		// so the api layer can report it as a diagnostic.
		var authErr *arr.AuthError
		if !errors.As(err, &authErr) {
			t.Fatalf("error %v is not an *arr.AuthError", err)
		}
		if authErr.Code != http.StatusUnauthorized {
			t.Errorf("AuthError.Code = %d, want 401", authErr.Code)
		}
		if authErr.Path != "/api/v3/system/status" || authErr.Method != http.MethodGet {
			t.Errorf("AuthError = %+v, want GET /api/v3/system/status", authErr)
		}
	})

	t.Run("forbidden", func(t *testing.T) {
		srv := arrmock.New(arrmock.Sonarr, "", arrmock.Fixtures{})
		defer srv.Close()
		srv.FailAll(403)

		c, err := arr.NewSonarrClient(srv.URL, srv.APIKey(), time.Second)
		if err != nil {
			t.Fatalf("NewSonarrClient: %v", err)
		}
		_, err = c.Series(ctx)
		if !errors.Is(err, arr.ErrAuth) {
			t.Fatalf("Series error = %v, want ErrAuth", err)
		}
		var authErr *arr.AuthError
		if !errors.As(err, &authErr) {
			t.Fatalf("error %v is not an *arr.AuthError", err)
		}
		if authErr.Code != http.StatusForbidden {
			t.Errorf("AuthError.Code = %d, want 403", authErr.Code)
		}
	})

	t.Run("decode failure", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("<html>not json</html>"))
		}))
		defer srv.Close()

		c, err := arr.NewRadarrClient(srv.URL, "k", time.Second)
		if err != nil {
			t.Fatalf("NewRadarrClient: %v", err)
		}
		if err := c.Ping(ctx); !errors.Is(err, arr.ErrDecode) {
			t.Fatalf("Ping error = %v, want ErrDecode", err)
		}
	})

	t.Run("unexpected status", func(t *testing.T) {
		srv := arrmock.New(arrmock.Radarr, "", arrmock.Fixtures{})
		defer srv.Close()
		srv.FailAll(500)

		c, err := arr.NewRadarrClient(srv.URL, srv.APIKey(), time.Second)
		if err != nil {
			t.Fatalf("NewRadarrClient: %v", err)
		}
		_, err = c.QualityProfiles(ctx)
		if !errors.Is(err, arr.ErrUnexpectedStatus) {
			t.Fatalf("error = %v, want ErrUnexpectedStatus", err)
		}
		var se *arr.StatusError
		if !errors.As(err, &se) {
			t.Fatalf("error %v is not a *StatusError", err)
		}
		if se.Code != 500 {
			t.Errorf("StatusError.Code = %d, want 500", se.Code)
		}
	})

	t.Run("network failure", func(t *testing.T) {
		srv := arrmock.New(arrmock.Radarr, "", arrmock.Fixtures{})
		c, err := arr.NewRadarrClient(srv.URL, srv.APIKey(), time.Second)
		if err != nil {
			t.Fatalf("NewRadarrClient: %v", err)
		}
		srv.Close()

		if _, err := c.Movies(ctx); !errors.Is(err, arr.ErrNetwork) {
			t.Fatalf("Movies error = %v, want ErrNetwork", err)
		}
	})

	t.Run("timeout is a network failure", func(t *testing.T) {
		srv := arrmock.New(arrmock.Sonarr, "", arrmock.Fixtures{})
		defer srv.Close()
		srv.SetDelay(200 * time.Millisecond)

		c, err := arr.NewSonarrClient(srv.URL, srv.APIKey(), 20*time.Millisecond)
		if err != nil {
			t.Fatalf("NewSonarrClient: %v", err)
		}
		if _, err := c.Series(ctx); !errors.Is(err, arr.ErrNetwork) {
			t.Fatalf("Series error = %v, want ErrNetwork", err)
		} else if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Series error = %v, want the underlying deadline preserved", err)
		}
	})

	t.Run("empty library decodes to empty slice", func(t *testing.T) {
		srv := arrmock.New(arrmock.Radarr, "", arrmock.Fixtures{})
		defer srv.Close()

		c, err := arr.NewRadarrClient(srv.URL, srv.APIKey(), time.Second)
		if err != nil {
			t.Fatalf("NewRadarrClient: %v", err)
		}
		movies, err := c.Movies(ctx)
		if err != nil {
			t.Fatalf("Movies: %v", err)
		}
		if movies == nil || len(movies) != 0 {
			t.Fatalf("movies = %#v, want empty non-nil slice", movies)
		}
	})
}

// TestPingRejectsCrossPointedInstance proves a client aimed at the wrong *arr
// application fails the connection test instead of reporting a healthy ping.
// The failure differs by direction and that is the point: neither product
// serves the other's base, so a Sonarr client pointed at a Radarr gets a 404
// and the loud API-version error, while an endpoint that does answer the Radarr
// base with Sonarr's appName is caught by the kind check.
func TestPingRejectsCrossPointedInstance(t *testing.T) {
	ctx := context.Background()

	t.Run("sonarr client against radarr", func(t *testing.T) {
		srv := arrmock.New(arrmock.Radarr, "", radarrFixtures())
		defer srv.Close()

		c, err := arr.NewSonarrClient(srv.URL, srv.APIKey(), time.Second)
		if err != nil {
			t.Fatalf("NewSonarrClient: %v", err)
		}
		err = c.Ping(ctx)
		var versionErr *arr.APIVersionError
		if !errors.As(err, &versionErr) {
			t.Fatalf("Ping error = %v, want an *arr.APIVersionError naming /api/v5", err)
		}
		if versionErr.APIBase != "/api/v5" || versionErr.Kind != arr.KindSonarr {
			t.Fatalf("APIVersionError = %+v, want the sonarr /api/v5 base", versionErr)
		}
	})

	t.Run("radarr client against sonarr", func(t *testing.T) {
		// The version cannot catch this direction: the fake answers the base
		// it is asked for, with Sonarr's appName and a healthy 200. Only the
		// kind check stands between that and a false "connected".
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"appName":"Sonarr","version":"4.0.0"}`))
		}))
		defer srv.Close()

		c, err := arr.NewRadarrClient(srv.URL, "k", time.Second)
		if err != nil {
			t.Fatalf("NewRadarrClient: %v", err)
		}
		err = c.Ping(ctx)
		if !errors.Is(err, arr.ErrKindMismatch) {
			t.Fatalf("Ping error = %v, want ErrKindMismatch", err)
		}
		var mismatch *arr.KindMismatchError
		if !errors.As(err, &mismatch) {
			t.Fatalf("error %v is not a *KindMismatchError", err)
		}
		if mismatch.Expected != arr.KindRadarr || mismatch.Got != "Sonarr" {
			t.Fatalf("mismatch = %+v", mismatch)
		}
	})

	t.Run("radarr client against a sonarr v5 fake", func(t *testing.T) {
		srv := arrmock.New(arrmock.Sonarr, "", sonarrFixtures())
		defer srv.Close()

		c, err := arr.NewRadarrClient(srv.URL, srv.APIKey(), time.Second)
		if err != nil {
			t.Fatalf("NewRadarrClient: %v", err)
		}
		// The fake serves v5 only, so the Radarr client's /api/v3 ping is a
		// 404 here — a loud version failure, never a silent pass.
		var versionErr *arr.APIVersionError
		if err := c.Ping(ctx); !errors.As(err, &versionErr) {
			t.Fatalf("Ping error = %v, want an *arr.APIVersionError", err)
		}
	})
}

func TestConstructorsRejectInvalidBaseURL(t *testing.T) {
	tests := []struct {
		name string
		url  string
	}{
		{name: "unparseable", url: "://nope"},
		{name: "missing scheme", url: "radarr:7878"},
		{name: "unsupported scheme", url: "ftp://radarr.local:7878"},
		{name: "missing host", url: "http://"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := arr.NewRadarrClient(tt.url, "k", time.Second); err == nil {
				t.Errorf("NewRadarrClient(%q) accepted an invalid base URL", tt.url)
			}
			if _, err := arr.NewSonarrClient(tt.url, "k", time.Second); err == nil {
				t.Errorf("NewSonarrClient(%q) accepted an invalid base URL", tt.url)
			}
		})
	}
}

func TestBaseURLTrailingSlashIsAccepted(t *testing.T) {
	srv := arrmock.New(arrmock.Radarr, "", arrmock.Fixtures{})
	defer srv.Close()

	c, err := arr.NewRadarrClient(srv.URL+"/", srv.APIKey(), time.Second)
	if err != nil {
		t.Fatalf("NewRadarrClient: %v", err)
	}
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

// TestMovieByIDReadsOneMovie covers the additive per-item read: GET
// /api/v3/movie/{id} returns the single movie, a missing id maps onto the shared
// error model, and the API key is sent as X-Api-Key but never echoed in an error.
func TestMovieByIDReadsOneMovie(t *testing.T) {
	ctx := context.Background()
	srv := arrmock.New(arrmock.Radarr, "per-item-radarr-key", arrmock.Fixtures{
		Movies: []arrmock.Movie{{
			ID: 7, Title: "Shrek", OriginalTitle: "Shrek (2001 original)", Year: 2001,
			TmdbID: 808, Path: "/media/movies/Shrek (2001)", FolderName: "Shrek (2001)",
			QualityProfileID: 4, Monitored: true, HasFile: true,
			OriginalLanguage: arrmock.Language{ID: 1, Name: "English"},
		}},
	})
	defer srv.Close()

	c, err := arr.NewRadarrClient(srv.URL, srv.APIKey(), time.Second)
	if err != nil {
		t.Fatalf("NewRadarrClient: %v", err)
	}
	movie, err := c.MovieByID(ctx, 7)
	if err != nil {
		t.Fatalf("MovieByID: %v", err)
	}
	if movie.ID != 7 || movie.Title != "Shrek" || movie.Year != 2001 {
		t.Errorf("movie = %+v, want Shrek (2001) id 7", movie)
	}
	if movie.OriginalTitle != "Shrek (2001 original)" {
		t.Errorf("original title = %q, want the arr originalTitle", movie.OriginalTitle)
	}
	if movie.Path != "/media/movies/Shrek (2001)" || movie.TmdbID != 808 {
		t.Errorf("movie path/tmdb = %q/%d", movie.Path, movie.TmdbID)
	}

	wantPath := "/api/v3/movie/7"
	var sawGet bool
	for _, r := range srv.Requests() {
		if r.Path != wantPath {
			continue
		}
		sawGet = true
		if r.Method != httpMethodGet {
			t.Errorf("%s method = %s, want GET", wantPath, r.Method)
		}
		if r.APIKey != srv.APIKey() {
			t.Errorf("%s X-Api-Key = %q, want the configured key", wantPath, r.APIKey)
		}
	}
	if !sawGet {
		t.Fatalf("no GET %s reached the instance: %+v", wantPath, srv.Requests())
	}

	_, err = c.MovieByID(ctx, 999)
	if !errors.Is(err, arr.ErrUnexpectedStatus) {
		t.Fatalf("MovieByID(999) error = %v, want ErrUnexpectedStatus", err)
	}
	var status *arr.StatusError
	if !errors.As(err, &status) || status.Code != http.StatusNotFound {
		t.Fatalf("MovieByID(999) error = %v, want a 404 StatusError", err)
	}
	if strings.Contains(err.Error(), srv.APIKey()) {
		t.Fatalf("error %q echoes the API key", err)
	}
}

// TestSeriesByIDReadsOneSeries is the Sonarr half of the per-item read: GET
// /api/v5/series/{id}, the 404 mapping and the key handling.
func TestSeriesByIDReadsOneSeries(t *testing.T) {
	ctx := context.Background()
	srv := arrmock.New(arrmock.Sonarr, "per-item-sonarr-key", arrmock.Fixtures{
		Series: []arrmock.Series{{
			ID: 3, Title: "The Expanse", OriginalTitle: "The Expanse (orig)", Year: 2015,
			TvdbID: 280619, Path: "/media/tv/The Expanse", FolderName: "The Expanse",
			QualityProfileID: 2, Monitored: true,
			OriginalLanguage: arrmock.Language{ID: 1, Name: "English"},
		}},
	})
	defer srv.Close()

	c, err := arr.NewSonarrClient(srv.URL, srv.APIKey(), time.Second)
	if err != nil {
		t.Fatalf("NewSonarrClient: %v", err)
	}
	show, err := c.SeriesByID(ctx, 3)
	if err != nil {
		t.Fatalf("SeriesByID: %v", err)
	}
	if show.ID != 3 || show.Title != "The Expanse" || show.TvdbID != 280619 {
		t.Errorf("series = %+v, want The Expanse (tvdb 280619)", show)
	}
	if show.OriginalTitle != "The Expanse (orig)" || show.Path != "/media/tv/The Expanse" {
		t.Errorf("series original title/path = %q/%q", show.OriginalTitle, show.Path)
	}

	_, err = c.SeriesByID(ctx, 99)
	if !errors.Is(err, arr.ErrUnexpectedStatus) {
		t.Fatalf("SeriesByID(99) error = %v, want ErrUnexpectedStatus", err)
	}
	var status *arr.StatusError
	if !errors.As(err, &status) || status.Code != http.StatusNotFound {
		t.Fatalf("SeriesByID(99) error = %v, want a 404 StatusError", err)
	}
	if strings.Contains(err.Error(), srv.APIKey()) {
		t.Fatalf("error %q echoes the API key", err)
	}
}

// TestPerItemReadsErrorModel pins the error mapping of the by-id reads and that
// no failure text can carry the API key.
func TestPerItemReadsErrorModel(t *testing.T) {
	ctx := context.Background()

	t.Run("auth failure", func(t *testing.T) {
		srv := arrmock.New(arrmock.Radarr, "correct-key", radarrFixtures())
		defer srv.Close()

		c, err := arr.NewRadarrClient(srv.URL, "wrong-key", time.Second)
		if err != nil {
			t.Fatalf("NewRadarrClient: %v", err)
		}
		if _, err := c.MovieByID(ctx, 7); !errors.Is(err, arr.ErrAuth) {
			t.Fatalf("MovieByID error = %v, want ErrAuth", err)
		}
	})

	t.Run("unexpected status", func(t *testing.T) {
		srv := arrmock.New(arrmock.Sonarr, "sonarr-key-value", sonarrFixtures())
		defer srv.Close()
		srv.FailAll(500)

		c, err := arr.NewSonarrClient(srv.URL, srv.APIKey(), time.Second)
		if err != nil {
			t.Fatalf("NewSonarrClient: %v", err)
		}
		_, err = c.SeriesByID(ctx, 3)
		if !errors.Is(err, arr.ErrUnexpectedStatus) {
			t.Fatalf("SeriesByID error = %v, want ErrUnexpectedStatus", err)
		}
		var status *arr.StatusError
		if !errors.As(err, &status) || status.Code != 500 {
			t.Fatalf("SeriesByID error = %v, want a 500 StatusError", err)
		}
		if strings.Contains(err.Error(), srv.APIKey()) {
			t.Fatalf("error %q echoes the API key", err)
		}
	})

	t.Run("network failure does not echo the key", func(t *testing.T) {
		const key = "very-distinctive-api-key-value"
		srv := arrmock.New(arrmock.Radarr, key, radarrFixtures())
		c, err := arr.NewRadarrClient(srv.URL, key, time.Second)
		if err != nil {
			t.Fatalf("NewRadarrClient: %v", err)
		}
		srv.Close()

		_, err = c.MovieByID(ctx, 7)
		if !errors.Is(err, arr.ErrNetwork) {
			t.Fatalf("MovieByID error = %v, want ErrNetwork", err)
		}
		if strings.Contains(err.Error(), key) {
			t.Fatalf("error %q echoes the API key", err)
		}
	})
}

const httpMethodGet = "GET"

// TestRadarrRescanMoviePostsCommand proves the rescan uses the Radarr v3
// RescanMovie command (movieId is not a field of DownloadedMoviesScan, whose
// only inputs are path/downloadClientId/importMode).
func TestRadarrRescanMoviePostsCommand(t *testing.T) {
	ctx := context.Background()
	srv := arrmock.New(arrmock.Radarr, "", radarrFixtures())
	defer srv.Close()

	c, err := arr.NewRadarrClient(srv.URL, srv.APIKey(), time.Second)
	if err != nil {
		t.Fatalf("NewRadarrClient: %v", err)
	}
	var client arr.Radarr = c
	if err := client.RescanMovie(ctx, 7); err != nil {
		t.Fatalf("RescanMovie: %v", err)
	}

	cmds := srv.Commands()
	if len(cmds) != 1 {
		t.Fatalf("commands = %+v, want exactly one", cmds)
	}
	if want := (arrmock.CommandRequest{Name: "RescanMovie", MovieID: 7}); cmds[0] != want {
		t.Errorf("command = %+v, want %+v", cmds[0], want)
	}
	reqs := srv.Requests()
	last := reqs[len(reqs)-1]
	if last.Method != http.MethodPost || last.Path != "/api/v3/command" {
		t.Errorf("last request = %s %s, want POST /api/v3/command", last.Method, last.Path)
	}
	if last.APIKey != srv.APIKey() {
		t.Errorf("X-Api-Key = %q, want the configured key", last.APIKey)
	}
}

func TestSonarrRescanSeriesPostsCommand(t *testing.T) {
	ctx := context.Background()
	srv := arrmock.New(arrmock.Sonarr, "", sonarrFixtures())
	defer srv.Close()

	c, err := arr.NewSonarrClient(srv.URL, srv.APIKey(), time.Second)
	if err != nil {
		t.Fatalf("NewSonarrClient: %v", err)
	}
	var client arr.Sonarr = c
	if err := client.RescanSeries(ctx, 3); err != nil {
		t.Fatalf("RescanSeries: %v", err)
	}

	cmds := srv.Commands()
	if len(cmds) != 1 {
		t.Fatalf("commands = %+v, want exactly one", cmds)
	}
	if want := (arrmock.CommandRequest{Name: "RescanSeries", SeriesID: 3}); cmds[0] != want {
		t.Errorf("command = %+v, want %+v", cmds[0], want)
	}
	reqs := srv.Requests()
	last := reqs[len(reqs)-1]
	if last.Method != http.MethodPost || last.Path != "/api/v5/command" {
		t.Errorf("last request = %s %s, want POST /api/v5/command", last.Method, last.Path)
	}
}

func TestRescanCommandErrorModel(t *testing.T) {
	ctx := context.Background()

	t.Run("auth failure", func(t *testing.T) {
		srv := arrmock.New(arrmock.Radarr, "", arrmock.Fixtures{})
		defer srv.Close()
		c, err := arr.NewRadarrClient(srv.URL, "wrong-key", time.Second)
		if err != nil {
			t.Fatalf("NewRadarrClient: %v", err)
		}
		if err := c.RescanMovie(ctx, 7); !errors.Is(err, arr.ErrAuth) {
			t.Fatalf("RescanMovie error = %v, want ErrAuth", err)
		}
		if got := len(srv.Commands()); got != 0 {
			t.Errorf("mock recorded %d commands after auth failure, want 0", got)
		}
	})

	t.Run("unexpected status", func(t *testing.T) {
		srv := arrmock.New(arrmock.Sonarr, "", arrmock.Fixtures{})
		defer srv.Close()
		srv.FailAll(500)
		c, err := arr.NewSonarrClient(srv.URL, srv.APIKey(), time.Second)
		if err != nil {
			t.Fatalf("NewSonarrClient: %v", err)
		}
		if err := c.RescanSeries(ctx, 3); !errors.Is(err, arr.ErrUnexpectedStatus) {
			t.Fatalf("RescanSeries error = %v, want ErrUnexpectedStatus", err)
		}
	})

	t.Run("network failure", func(t *testing.T) {
		srv := arrmock.New(arrmock.Radarr, "", arrmock.Fixtures{})
		c, err := arr.NewRadarrClient(srv.URL, srv.APIKey(), time.Second)
		if err != nil {
			t.Fatalf("NewRadarrClient: %v", err)
		}
		srv.Close()
		if err := c.RescanMovie(ctx, 7); !errors.Is(err, arr.ErrNetwork) {
			t.Fatalf("RescanMovie error = %v, want ErrNetwork", err)
		}
	})
}
