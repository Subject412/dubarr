package arr_test

import (
	"context"
	"testing"
	"time"

	"github.com/Subject412/dubarr/backend/internal/arr"
	"github.com/Subject412/dubarr/backend/internal/arr/arrmock"
)

// TestSonarrClientReadsEpisodesAndFiles pins the M6-1 payload shapes: an
// episode carries its ids and nested episodeFile (target video path), and a
// Radarr movie carries its nested movieFile.
func TestSonarrClientReadsEpisodesAndFiles(t *testing.T) {
	ctx := context.Background()
	srv := arrmock.New(arrmock.Sonarr, "", arrmock.Fixtures{
		Series: []arrmock.Series{
			{ID: 3, Title: "The Expanse", Year: 2015, Path: "/media/tv/The Expanse",
				OriginalLanguage: arrmock.Language{ID: 1, Name: "English"}},
		},
		Episodes: []arrmock.Episode{
			{
				ID: 42, SeriesID: 3, SeasonNumber: 1, EpisodeNumber: 1,
				Title: "Dulcinea", AirDateUTC: "2015-12-14T00:00:00Z", Monitored: true, HasFile: true,
				EpisodeFile: &arrmock.EpisodeFile{
					ID: 7, SeriesID: 3, SeasonNumber: 1,
					RelativePath: "Season 01/S01E01.mkv",
					Path:         "/media/tv/The Expanse/Season 01/S01E01.mkv",
					Size:         1234,
				},
			},
			{
				ID: 43, SeriesID: 3, SeasonNumber: 1, EpisodeNumber: 2,
				Title: "The Big Empty", Monitored: true, HasFile: false,
			},
			// A sibling series' episode must be filtered out by the fake.
			{ID: 99, SeriesID: 5, SeasonNumber: 2, EpisodeNumber: 2, Title: "Elsewhere", HasFile: true},
		},
	})
	defer srv.Close()

	c, err := arr.NewSonarrClient(srv.URL, srv.APIKey(), 2*time.Second)
	if err != nil {
		t.Fatalf("NewSonarrClient: %v", err)
	}
	episodes, err := c.Episodes(ctx, 3)
	if err != nil {
		t.Fatalf("Episodes: %v", err)
	}
	if len(episodes) != 2 {
		t.Fatalf("got %d episodes, want 2 (seriesId filtered)", len(episodes))
	}
	first := episodes[0]
	if first.ID != 42 || first.SeriesID != 3 || first.SeasonNumber != 1 || first.EpisodeNumber != 1 {
		t.Errorf("episode ids = %+v", first)
	}
	if !first.HasFile || first.EpisodeFile == nil || first.EpisodeFile.Path != "/media/tv/The Expanse/Season 01/S01E01.mkv" {
		t.Errorf("episode file = %+v", first.EpisodeFile)
	}
	if episodes[1].HasFile || episodes[1].EpisodeFile != nil {
		t.Errorf("episode without file = %+v", episodes[1])
	}
}

func TestRadarrClientReadsMovieFile(t *testing.T) {
	ctx := context.Background()
	srv := arrmock.New(arrmock.Radarr, "", arrmock.Fixtures{
		Movies: []arrmock.Movie{
			{
				ID: 7, Title: "Shrek", Year: 2001, TmdbID: 808,
				Path: "/media/movies/Shrek (2001)", HasFile: true,
				OriginalLanguage: arrmock.Language{ID: 1, Name: "English"},
				MovieFile: &arrmock.MovieFile{
					ID: 3, MovieID: 7, RelativePath: "Shrek (2001) [ArcA310].mkv",
					Path: "/media/movies/Shrek (2001)/Shrek (2001) [ArcA310].mkv", Size: 99,
				},
			},
			{
				ID: 9, Title: "Minions", Year: 2015, HasFile: false,
				OriginalLanguage: arrmock.Language{ID: 1, Name: "English"},
			},
		},
	})
	defer srv.Close()

	c, err := arr.NewRadarrClient(srv.URL, srv.APIKey(), 2*time.Second)
	if err != nil {
		t.Fatalf("NewRadarrClient: %v", err)
	}
	movies, err := c.Movies(ctx)
	if err != nil {
		t.Fatalf("Movies: %v", err)
	}
	if len(movies) != 2 {
		t.Fatalf("got %d movies, want 2", len(movies))
	}
	if movies[0].MovieFile == nil || movies[0].MovieFile.Path != "/media/movies/Shrek (2001)/Shrek (2001) [ArcA310].mkv" {
		t.Errorf("movieFile = %+v", movies[0].MovieFile)
	}
	if movies[1].MovieFile != nil {
		t.Errorf("movie without file carries movieFile %+v", movies[1].MovieFile)
	}
}
