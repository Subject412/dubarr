package arr_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Subject412/dubarr/backend/internal/arr"
	"github.com/Subject412/dubarr/backend/internal/arr/arrmock"
)

// tagFixtures carries the {base}/tag payload both products expose.
func tagFixtures() arrmock.Fixtures {
	return arrmock.Fixtures{
		Tags: []arrmock.Tag{
			{ID: 1, Label: "dub"},
			{ID: 4, Label: "Polish Dubbing"},
		},
	}
}

// TestTagsReadsFromMock pins Tags() against the fake for both products: same
// endpoint, same shape, sent with X-Api-Key.
func TestTagsReadsFromMock(t *testing.T) {
	ctx := context.Background()
	for _, kind := range []arrmock.Kind{arrmock.Radarr, arrmock.Sonarr} {
		t.Run(string(kind), func(t *testing.T) {
			srv := arrmock.New(kind, "", tagFixtures())
			defer srv.Close()

			var c arr.Client
			var err error
			if kind == arrmock.Radarr {
				c, err = arr.NewRadarrClient(srv.URL, srv.APIKey(), 2*time.Second)
			} else {
				c, err = arr.NewSonarrClient(srv.URL, srv.APIKey(), 2*time.Second)
			}
			if err != nil {
				t.Fatalf("new client: %v", err)
			}

			tags, err := c.Tags(ctx)
			if err != nil {
				t.Fatalf("Tags: %v", err)
			}
			if len(tags) != 2 {
				t.Fatalf("got %d tags, want 2", len(tags))
			}
			if tags[0] != (arr.Tag{ID: 1, Label: "dub"}) || tags[1] != (arr.Tag{ID: 4, Label: "Polish Dubbing"}) {
				t.Errorf("tags = %+v", tags)
			}

			// Mock parity: the fake saw GET {apiBase}/tag with the key. Each
			// product is asked at its own base — Sonarr v5, Radarr v3.
			wantPath := "/api/v3/tag"
			if kind == arrmock.Sonarr {
				wantPath = "/api/v5/tag"
			}
			reqs := srv.Requests()
			last := reqs[len(reqs)-1]
			if last.Method != "GET" || last.Path != wantPath {
				t.Errorf("last request = %s %s, want GET %s", last.Method, last.Path, wantPath)
			}
			if last.APIKey != srv.APIKey() {
				t.Errorf("X-Api-Key = %q, want the configured key", last.APIKey)
			}
		})
	}
}

// TestTagsEmpty pins the configured-but-empty instance: an empty array decodes
// as zero tags, not an error.
func TestTagsEmpty(t *testing.T) {
	srv := arrmock.New(arrmock.Radarr, "", arrmock.Fixtures{})
	defer srv.Close()

	c, err := arr.NewRadarrClient(srv.URL, srv.APIKey(), 2*time.Second)
	if err != nil {
		t.Fatalf("NewRadarrClient: %v", err)
	}
	tags, err := c.Tags(context.Background())
	if err != nil {
		t.Fatalf("Tags: %v", err)
	}
	if tags == nil || len(tags) != 0 {
		t.Fatalf("tags = %#v, want an empty non-nil slice", tags)
	}
}

// TestTagsErrors pins the shared error model: auth, unexpected status and
// network failures are classified like every other client call.
func TestTagsErrors(t *testing.T) {
	ctx := context.Background()

	t.Run("auth failure", func(t *testing.T) {
		srv := arrmock.New(arrmock.Sonarr, "", tagFixtures())
		defer srv.Close()
		c, err := arr.NewSonarrClient(srv.URL, "wrong-key", time.Second)
		if err != nil {
			t.Fatalf("NewSonarrClient: %v", err)
		}
		if _, err := c.Tags(ctx); !errors.Is(err, arr.ErrAuth) {
			t.Fatalf("Tags error = %v, want ErrAuth", err)
		}
	})

	t.Run("unexpected status", func(t *testing.T) {
		srv := arrmock.New(arrmock.Radarr, "", tagFixtures())
		defer srv.Close()
		srv.FailAll(500)
		c, err := arr.NewRadarrClient(srv.URL, srv.APIKey(), time.Second)
		if err != nil {
			t.Fatalf("NewRadarrClient: %v", err)
		}
		if _, err := c.Tags(ctx); !errors.Is(err, arr.ErrUnexpectedStatus) {
			t.Fatalf("Tags error = %v, want ErrUnexpectedStatus", err)
		}
	})

	t.Run("network failure", func(t *testing.T) {
		srv := arrmock.New(arrmock.Radarr, "", tagFixtures())
		c, err := arr.NewRadarrClient(srv.URL, srv.APIKey(), time.Second)
		if err != nil {
			t.Fatalf("NewRadarrClient: %v", err)
		}
		srv.Close()
		if _, err := c.Tags(ctx); !errors.Is(err, arr.ErrNetwork) {
			t.Fatalf("Tags error = %v, want ErrNetwork", err)
		}
	})
}
