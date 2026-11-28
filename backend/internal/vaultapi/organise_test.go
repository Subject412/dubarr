package vaultapi

import (
	"net/http"
	"path/filepath"
	"testing"
)

// TestOrganisePreviewRendersIDs is U07's end-to-end path over HTTP: a movie
// whose vault_media row carries tmdb 129 previews a destination with a bare
// "[129]", and a series with no IMDb id in the mirror drops the token instead of
// leaving "[]" or "()" behind.
func TestOrganisePreviewRendersIDs(t *testing.T) {
	h := newOrganiseHarness(t)
	movieID, movieFolder := h.media("movie", "Spirited Away", 2001, 129)
	h.track(movieID, movieFolder, "legacy name.pl.mka", "dubbing", "eac3", "5.1", "pl", nil, nil)
	h.setNaming("{Title} ({Year}) [{TmdbId}][{ImdbId}]", "{Title} [{TvdbId}]", "")

	status, plan, body := h.previewAll()
	if status != http.StatusOK {
		t.Fatalf("preview = %d %s", status, body)
	}
	if len(plan.Files) != 1 {
		t.Fatalf("files = %d, want 1: %s", len(plan.Files), body)
	}
	// No mirror row exists, so {ImdbId} is unknown: the empty group goes with it.
	want := filepath.Join(movieFolder, "Spirited Away (2001) [129].pl.mka")
	if got := plan.Files[0].Destination; got != want {
		t.Fatalf("destination = %q, want %q", got, want)
	}
}
