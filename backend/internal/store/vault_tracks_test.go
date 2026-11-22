package store

import (
	"context"
	"testing"
)

func TestVaultTrackByID(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	season, episode := 1, 2
	mediaID, err := s.EnsureVaultMedia(ctx, VaultMediaRecord{
		MediaType:  "series",
		Title:      "Show",
		FolderPath: "/vault/Show",
	})
	if err != nil {
		t.Fatalf("EnsureVaultMedia: %v", err)
	}
	id, err := s.RecordVaultTrack(ctx, VaultTrackRecord{
		VaultMediaID:  mediaID,
		SeasonNumber:  &season,
		EpisodeNumber: &episode,
		GroupUUID:     "g",
		LangISO2:      "pl",
		LangISO3:      "pol",
		Variant:       "dubbing",
		FilePath:      "/vault/Show.S01E02.pl.mka",
		SHA256:        "abc",
		Codec:         "eac3",
		Channels:      "5.1",
		ChannelCount:  6,
		BitrateBPS:    640000,
		SampleRate:    48000,
		DurationMS:    1234,
	})
	if err != nil {
		t.Fatalf("RecordVaultTrack: %v", err)
	}

	rec, found, err := s.VaultTrackByID(ctx, id)
	if err != nil || !found {
		t.Fatalf("VaultTrackByID = (found=%v, err=%v)", found, err)
	}
	if rec.SeasonNumber == nil || *rec.SeasonNumber != 1 || rec.EpisodeNumber == nil || *rec.EpisodeNumber != 2 {
		t.Errorf("season/episode = %v/%v, want 1/2", rec.SeasonNumber, rec.EpisodeNumber)
	}
	if rec.FilePath != "/vault/Show.S01E02.pl.mka" || rec.SHA256 != "abc" || rec.LangISO3 != "pol" {
		t.Errorf("record = %+v", rec)
	}
	if rec.BitrateBPS != 640000 || rec.SampleRate != 48000 || rec.DurationMS != 1234 {
		t.Errorf("numeric fields = %+v", rec)
	}

	if _, found, err := s.VaultTrackByID(ctx, 4242); err != nil || found {
		t.Errorf("unknown id = (found=%v, err=%v), want (false, nil)", found, err)
	}
}
