package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// VaultMediaRecord identifies the movie/series row an archived audio track
// belongs to (schema table 6, vault_media). TMDBID/TVDBID of 0 are stored as
// NULL. FolderPath is the vault folder holding the media's tracks.
type VaultMediaRecord struct {
	// MediaType is "movie" or "series".
	MediaType string
	Title     string
	Year      int
	TMDBID    int64
	TVDBID    int64
	// OriginalLanguageISO2 is the library's original language, when known.
	OriginalLanguageISO2 string
	FolderPath           string
}

// VaultTrackRecord is one immutable archived .mka (schema table 7,
// audio_tracks). SeasonNumber/EpisodeNumber are nil for movies. LangISO2 is the
// filename suffix language, LangISO3 the ISO 639-2/B code when known.
type VaultTrackRecord struct {
	VaultMediaID      int64
	SeasonNumber      *int
	EpisodeNumber     *int
	GroupUUID         string
	LangISO2          string
	LangISO3          string
	Variant           string
	IsOriginalLang    bool
	FilePath          string
	SHA256            string
	Codec             string
	Channels          string
	ChannelCount      int
	BitrateBPS        int
	SampleRate        int
	SourceFPS         float64
	DurationMS        int64
	SourceReleaseName string
	Notes             string
}

// EnsureVaultMedia returns the id of the vault_media row for the record,
// inserting it when absent. Identity is (media_type, tmdb_id, tvdb_id); 0 ids
// are matched as SQL NULL so a movie/series without an id is still found on the
// next call. The read and the insert share a transaction, so concurrent callers
// cannot create duplicates.
func (s *Store) EnsureVaultMedia(ctx context.Context, m VaultMediaRecord) (int64, error) {
	if m.MediaType != "movie" && m.MediaType != "series" {
		return 0, fmt.Errorf("store: vault media type %q is not movie/series", m.MediaType)
	}
	if m.Title == "" {
		return 0, errors.New("store: vault media title is required")
	}

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: begin vault media: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed

	tmdb, tvdb := nullableID(m.TMDBID), nullableID(m.TVDBID)
	var id int64
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM vault_media
		 WHERE media_type = ? AND tmdb_id IS ? AND tvdb_id IS ?`,
		m.MediaType, tmdb, tvdb).Scan(&id)
	switch {
	case err == nil:
		return id, nil
	case !errors.Is(err, sql.ErrNoRows):
		return 0, fmt.Errorf("store: read vault media: %w", err)
	}

	res, err := tx.ExecContext(ctx,
		`INSERT INTO vault_media
		   (media_type, title, year, tmdb_id, tvdb_id, original_language_iso2, vault_folder_path)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		m.MediaType, m.Title, nullableInt(m.Year), tmdb, tvdb,
		nullableText(m.OriginalLanguageISO2), m.FolderPath)
	if err != nil {
		return 0, fmt.Errorf("store: insert vault media: %w", err)
	}
	id, err = res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: vault media id: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: commit vault media: %w", err)
	}
	return id, nil
}

// RecordVaultTrack inserts an audio_tracks row and returns its id. file_path is
// UNIQUE: a second insert for the same path fails rather than replacing the
// archived track (write-once).
func (s *Store) RecordVaultTrack(ctx context.Context, t VaultTrackRecord) (int64, error) {
	res, err := s.DB.ExecContext(ctx,
		`INSERT INTO audio_tracks
		   (vault_media_id, season_number, episode_number, group_uuid, lang_iso2, lang_iso3,
		    variant, is_original_lang, file_path, sha256, codec, channels, channel_count,
		    bitrate_bps, sample_rate, source_fps, duration_ms, source_release_name, notes)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.VaultMediaID, t.SeasonNumber, t.EpisodeNumber, t.GroupUUID, t.LangISO2,
		nullableText(t.LangISO3), t.Variant, t.IsOriginalLang, t.FilePath, t.SHA256,
		t.Codec, t.Channels, t.ChannelCount, nullableInt(t.BitrateBPS),
		nullableInt(t.SampleRate), t.SourceFPS, t.DurationMS,
		nullableText(t.SourceReleaseName), nullableText(t.Notes))
	if err != nil {
		return 0, fmt.Errorf("store: record vault track %s: %w", t.FilePath, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: vault track id: %w", err)
	}
	return id, nil
}

// VaultTrackListing is one archived track joined with its catalog row, as
// shown by the read-only vault UI.
type VaultTrackListing struct {
	ID                int64
	MediaType         string
	Title             string
	SeasonNumber      *int
	EpisodeNumber     *int
	FilePath          string
	SHA256            string
	LangISO2          string
	LangISO3          string
	Variant           string
	Codec             string
	Channels          string
	ChannelCount      int
	SourceReleaseName string
	CreatedAt         string
}

// ListVaultTracks returns every archived audio track, newest first, with the
// vault_media catalog row attached. It is strictly read-only: the vault itself
// is write-once and never modified through this path.
func (s *Store) ListVaultTracks(ctx context.Context) ([]VaultTrackListing, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT t.id, m.media_type, m.title, t.season_number, t.episode_number,
		       t.file_path, t.sha256, t.lang_iso2, COALESCE(t.lang_iso3, ''),
		       t.variant, t.codec, t.channels, t.channel_count,
		       COALESCE(t.source_release_name, ''), CAST(t.created_at AS TEXT)
		FROM audio_tracks t
		JOIN vault_media m ON m.id = t.vault_media_id
		ORDER BY t.created_at DESC, t.id DESC`)
	if err != nil {
		return nil, fmt.Errorf("store: list vault tracks: %w", err)
	}
	defer rows.Close()

	out := []VaultTrackListing{}
	for rows.Next() {
		var (
			l               VaultTrackListing
			season, episode sql.NullInt64
		)
		if err := rows.Scan(&l.ID, &l.MediaType, &l.Title, &season, &episode,
			&l.FilePath, &l.SHA256, &l.LangISO2, &l.LangISO3,
			&l.Variant, &l.Codec, &l.Channels, &l.ChannelCount,
			&l.SourceReleaseName, &l.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scan vault track: %w", err)
		}
		if season.Valid {
			v := int(season.Int64)
			l.SeasonNumber = &v
		}
		if episode.Valid {
			v := int(episode.Int64)
			l.EpisodeNumber = &v
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate vault tracks: %w", err)
	}
	return out, nil
}

func nullableID(id int64) any {
	if id <= 0 {
		return nil
	}
	return id
}

func nullableInt(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

func nullableText(s string) any {
	if s == "" {
		return nil
	}
	return s
}
