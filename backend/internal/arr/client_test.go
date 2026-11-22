package arr_test

import (
	"context"
	"errors"
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
				if got != want {
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
		if err := c.Ping(ctx); !errors.Is(err, arr.ErrAuth) {
			t.Fatalf("Ping error = %v, want ErrAuth", err)
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
		if _, err := c.Series(ctx); !errors.Is(err, arr.ErrAuth) {
			t.Fatalf("Series error = %v, want ErrAuth", err)
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

const httpMethodGet = "GET"
