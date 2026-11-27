-- 0009: indexes for the library hierarchy reads (#56).
--
-- #56 replaces the one flat Library listing with Radarr's flat movie list and
-- Sonarr's series -> seasons -> episodes tree, each level read from
-- arr_library_cache by its own query. Those reads have three shapes the
-- surviving 0001 indexes do not cover, so each gets one index here:
--
--   * seasons and episodes are read by series: idx_arr_cache_series_tree keys
--     the tree walk on (series_id, season_number, episode_number), so
--     EpisodesForSeason and SeasonsForSeries never scan the cache and a season
--     arrives already ordered.
--   * the movie and series lists page a filtered set in title order:
--     idx_arr_cache_media_title keys (media_type, title, id) so the page ORDER
--     BY is index-backed and the id tiebreaker keeps paging deterministic
--     while arrsync rewrites the snapshot.
--   * the same lists can be ordered by recency: idx_arr_cache_media_updated
--     keys (media_type, updated_at DESC) so sort=updated does not sort the
--     whole cache per request.
--
-- Indexes only: no table rebuild, no column change, no data migration, and
-- 0001-0008 stay untouched. Creating an index over an existing cache is a
-- one-off cost at boot; every statement below is idempotent.

CREATE INDEX IF NOT EXISTS idx_arr_cache_series_tree
    ON arr_library_cache(series_id, season_number, episode_number);

CREATE INDEX IF NOT EXISTS idx_arr_cache_media_title
    ON arr_library_cache(media_type, title COLLATE NOCASE, id);

CREATE INDEX IF NOT EXISTS idx_arr_cache_media_updated
    ON arr_library_cache(media_type, updated_at DESC);
