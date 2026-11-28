package arr_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Subject412/dubarr/backend/internal/arr"
	"github.com/Subject412/dubarr/backend/internal/arr/arrmock"
)

// TestParseRadarrNormalisesShape pins Radarr's parse payload mapping: the
// release name stays the top-level title, so the media title comes from
// parsedMovieInfo, with the quality name and year alongside.
func TestParseRadarrNormalisesShape(t *testing.T) {
	ctx := context.Background()
	srv := arrmock.New(arrmock.Radarr, "", arrmock.Fixtures{
		Parse: arrmock.ParseResponse{Title: "Shrek", Year: 2001, Quality: "Bluray-1080p"},
	})
	defer srv.Close()

	c, err := arr.NewRadarrClient(srv.URL, srv.APIKey(), 2*time.Second)
	if err != nil {
		t.Fatalf("NewRadarrClient: %v", err)
	}
	res, err := c.Parse(ctx, "Shrek.2001.1080p.BluRay.x264-GRP")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if res.Title != "Shrek" {
		t.Errorf("Title = %q, want Shrek", res.Title)
	}
	if res.Year != 2001 {
		t.Errorf("Year = %d, want 2001", res.Year)
	}
	if res.Quality != "Bluray-1080p" {
		t.Errorf("Quality = %q, want Bluray-1080p", res.Quality)
	}
	// A parsed title, a year and a quality: the highest deterministic score.
	if res.Confidence != 1 {
		t.Errorf("Confidence = %v, want 1", res.Confidence)
	}
	var raw map[string]any
	if err := json.Unmarshal(res.Raw, &raw); err != nil {
		t.Fatalf("Raw is not JSON: %v (%s)", err, res.Raw)
	}
	if _, ok := raw["parsedMovieInfo"]; !ok {
		t.Errorf("Raw does not carry the Radarr payload: %s", res.Raw)
	}

	// The release name was posted as the request title, and the key was sent.
	got := srv.ParseRequests()
	if len(got) != 1 || got[0] != "Shrek.2001.1080p.BluRay.x264-GRP" {
		t.Fatalf("parse requests = %v", got)
	}
	var sawParse bool
	for _, req := range srv.Requests() {
		if req.Path == "/api/v3/parse" && req.Method == "POST" {
			sawParse = true
			if req.APIKey != srv.APIKey() {
				t.Errorf("X-Api-Key = %q, want the configured key", req.APIKey)
			}
		}
	}
	if !sawParse {
		t.Errorf("no POST /api/v3/parse request was recorded: %+v", srv.Requests())
	}
}

// TestParseSonarrNormalisesShape pins Sonarr's payload mapping: the series title
// and its year live under parsedEpisodeInfo/series, and the result carries the
// same shape as Radarr's.
func TestParseSonarrNormalisesShape(t *testing.T) {
	ctx := context.Background()
	srv := arrmock.New(arrmock.Sonarr, "", arrmock.Fixtures{
		Parse: arrmock.ParseResponse{SeriesTitle: "The Expanse", SeriesYear: 2015, Quality: "WEBDL-1080p"},
	})
	defer srv.Close()

	c, err := arr.NewSonarrClient(srv.URL, srv.APIKey(), 2*time.Second)
	if err != nil {
		t.Fatalf("NewSonarrClient: %v", err)
	}
	res, err := c.Parse(ctx, "The.Expanse.S01E01.1080p.WEB-DL")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if res.Title != "The Expanse" {
		t.Errorf("Title = %q, want The Expanse", res.Title)
	}
	if res.Year != 2015 {
		t.Errorf("Year = %d, want 2015", res.Year)
	}
	if res.Quality != "WEBDL-1080p" {
		t.Errorf("Quality = %q, want WEBDL-1080p", res.Quality)
	}
	if res.Confidence != 1 {
		t.Errorf("Confidence = %v, want 1", res.Confidence)
	}

	// An instance that parses nothing still answers 200: an empty verdict with
	// zero confidence, not an error.
	empty := arrmock.New(arrmock.Sonarr, "", arrmock.Fixtures{})
	defer empty.Close()
	emptyClient, err := arr.NewSonarrClient(empty.URL, empty.APIKey(), 2*time.Second)
	if err != nil {
		t.Fatalf("NewSonarrClient: %v", err)
	}
	unparsed, err := emptyClient.Parse(ctx, "kraina.lodu.2013.pl")
	if err != nil {
		t.Fatalf("Parse unparsed: %v", err)
	}
	if unparsed.Title != "" || unparsed.Confidence != 0 {
		t.Errorf("unparsed result = %+v, want an empty verdict", unparsed)
	}
	if unparsed.Parsed {
		t.Errorf("unparsed result reports Parsed = true: %+v", unparsed)
	}
}

// TestParseErrorModel pins that parse fails through the shared error model on
// both products, each at its own base, so the identify route can classify it,
// and that the API key never appears in an error.
func TestParseErrorModel(t *testing.T) {
	ctx := context.Background()
	const apiKey = "parse-secret-key-do-not-leak"

	t.Run("auth failure", func(t *testing.T) {
		srv := arrmock.New(arrmock.Radarr, apiKey, arrmock.Fixtures{})
		defer srv.Close()
		c, err := arr.NewRadarrClient(srv.URL, "wrong-key", time.Second)
		if err != nil {
			t.Fatalf("NewRadarrClient: %v", err)
		}
		_, err = c.Parse(ctx, "Shrek.2001.1080p")
		if !errors.Is(err, arr.ErrAuth) {
			t.Fatalf("Parse error = %v, want ErrAuth", err)
		}
		if strings.Contains(err.Error(), "wrong-key") || strings.Contains(err.Error(), apiKey) {
			t.Errorf("error leaks a key: %v", err)
		}
	})

	t.Run("unexpected status", func(t *testing.T) {
		srv := arrmock.New(arrmock.Sonarr, apiKey, arrmock.Fixtures{})
		defer srv.Close()
		srv.FailAll(500)
		c, err := arr.NewSonarrClient(srv.URL, apiKey, time.Second)
		if err != nil {
			t.Fatalf("NewSonarrClient: %v", err)
		}
		_, err = c.Parse(ctx, "The.Expanse.S01E01")
		if !errors.Is(err, arr.ErrUnexpectedStatus) {
			t.Fatalf("Parse error = %v, want ErrUnexpectedStatus", err)
		}
		var status *arr.StatusError
		if !errors.As(err, &status) || status.Code != 500 {
			t.Errorf("status error = %v, want HTTP 500", err)
		}
		if strings.Contains(err.Error(), apiKey) {
			t.Errorf("error leaks the key: %v", err)
		}
	})

	t.Run("network failure", func(t *testing.T) {
		srv := arrmock.New(arrmock.Radarr, apiKey, arrmock.Fixtures{})
		c, err := arr.NewRadarrClient(srv.URL, apiKey, time.Second)
		if err != nil {
			t.Fatalf("NewRadarrClient: %v", err)
		}
		srv.Close()
		_, err = c.Parse(ctx, "Shrek.2001.1080p")
		if !errors.Is(err, arr.ErrNetwork) {
			t.Fatalf("Parse error = %v, want ErrNetwork", err)
		}
		if strings.Contains(err.Error(), apiKey) {
			t.Errorf("error leaks the key: %v", err)
		}
	})

	// A timeout is a network-class failure at the client: the caller's context
	// deadline is what turns it into the operator-facing "timeout" class.
	t.Run("timeout", func(t *testing.T) {
		srv := arrmock.New(arrmock.Radarr, apiKey, arrmock.Fixtures{})
		defer srv.Close()
		srv.SetDelay(300 * time.Millisecond)
		c, err := arr.NewRadarrClient(srv.URL, apiKey, 50*time.Millisecond)
		if err != nil {
			t.Fatalf("NewRadarrClient: %v", err)
		}
		_, err = c.Parse(ctx, "Shrek.2001.1080p")
		if !errors.Is(err, arr.ErrNetwork) {
			t.Fatalf("Parse timeout error = %v, want ErrNetwork", err)
		}
		if strings.Contains(err.Error(), apiKey) {
			t.Errorf("error leaks the key: %v", err)
		}
	})

	t.Run("empty title", func(t *testing.T) {
		srv := arrmock.New(arrmock.Radarr, apiKey, arrmock.Fixtures{})
		defer srv.Close()
		c, err := arr.NewRadarrClient(srv.URL, apiKey, time.Second)
		if err != nil {
			t.Fatalf("NewRadarrClient: %v", err)
		}
		if _, err := c.Parse(ctx, "   "); err == nil {
			t.Fatal("Parse with a blank title returned no error")
		}
		if len(srv.Requests()) != 0 {
			t.Errorf("a blank title still hit the instance: %+v", srv.Requests())
		}
	})
}
