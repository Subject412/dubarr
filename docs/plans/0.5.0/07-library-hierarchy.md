# 07 — #56 Library hierarchy: series → seasons → episodes

Branch: `feat/56-library-hierarchy` · Merge position: 7 of 8 · Depends on: nothing in 0.5.0 (reads the existing `arr_library_cache`) · Blocks: nothing · Settings: none

## Goal

The Library screen is four flat routes over one flat table, so a 12 000-item
Sonarr library arrives as 12 000 sibling rows and the browser is asked to group
them. #56 makes the screen behave the way the arrs it mirrors behave: **Radarr
is flat movies, Sonarr is series → seasons → episodes**, each level fetched
from the server, with server-side search and paging behind hard caps. The
endpoint set is read-only by construction — it must never write the arr mirror,
because the mirror's contents are owned by `arrsync` (and, indirectly, by the
two arrs themselves).

## Design

### Shape of the tree

```
Radarr tab                                   Sonarr tab
GET /api/library/movies?q&page&pageSize      GET /api/library/series?q&page&pageSize
  └ MovieResource[]                            └ SeriesResource[]  (one row per series_id)
                                                 └ GET /api/library/series/{id}/seasons
                                                     └ SeasonResource[] (aggregated)
                                                        └ GET /api/library/series/{id}/seasons/{n}/episodes
                                                            └ EpisodeResource[] (that season, one page)
```

Three requests to open one episode list; **never** "fetch all episodes and
group in the client". A season is small enough to return whole (hard cap
below), a series list is not.

### Local C# resource shapes to imitate

Read these for field naming and nesting, do not port their behaviour:

- `/home/gwdawid/Dokumenty/GitHub/Sonarr/src/Sonarr.Api.V3/Series/SeriesController.cs`
- `/home/gwdawid/Dokumenty/GitHub/Sonarr/src/Sonarr.Api.V3/Series/SeasonResource.cs`
- `/home/gwdawid/Dokumenty/GitHub/Sonarr/src/Sonarr.Api.V3/Series/SeasonStatisticsResource.cs`
- `/home/gwdawid/Dokumenty/GitHub/Sonarr/src/Sonarr.Api.V3/Episodes/EpisodeController.cs`
- `/home/gwdawid/Dokumenty/GitHub/Sonarr/src/Sonarr.Api.V3/Episodes/EpisodeResource.cs`
- `/home/gwdawid/Dokumenty/GitHub/Radarr/src/Radarr.Api.V3/Movies/MovieResource.cs` (and `MovieController.cs`)

What we take: `id`, `title`, `year`, `path`, `sizeOnDisk`, `hasFile`,
`seasonNumber`, `episodeNumber`, `statistics { episodeFileCount, episodeCount,
percentOfEpisodes }`, and the nesting `series → seasons[] → statistics`.
What we deliberately do **not** take: `monitored`, `qualityProfileId`,
`images`, `ratings`, `overview`, `genres`, `added`, `status`, `calendar`,
`wanted/missing`, `seasonPass`, `rename` endpoints. See the out-of-scope list.

### Go contract — `backend/internal/store/library_hierarchy.go` (new)

`arr_library_cache` already carries everything needed (0001 columns, 0004
metadata): `media_type` (`movie`|`episode`), `external_item_id`, `series_id`,
`title`, `original_title`, `year`, `season_number`, `episode_number`,
`episode_title`, `media_folder_path`, `target_file_path`, `eligibility_status`,
`root_folder_path`, `updated_at`. No new columns; one migration adds indexes.

```go
package store

// HierarchyPage is the paging envelope shared by the movie and series lists.
// Page is 1-based; PageSize is clamped to [1, LibraryPageMaxLimit] and 0 means
// LibraryPageDefaultLimit — the same bounds the flat route already enforces.
type HierarchyPage struct {
    Page     int   `json:"page"`
    PageSize int   `json:"page_size"`
    Total    int64 `json:"total"`
    Items    any   `json:"items"` // []MovieRow | []SeriesRow; concrete in handlers
}

// HierarchyFilter is the whole query surface. Every field is server-side.
type HierarchyFilter struct {
    Q          string // case-insensitive substring on title/original_title/episode_title
    InstanceID int64  // <=0 means every arr instance
    Page       int
    PageSize   int
    Sort       string // "title" (default) | "year" | "updated"
    Dir        string // "asc" (default) | "desc"
}

type MovieRow struct {
    ID               int64  // arr_library_cache.id — the id the rest of the API uses
    ExternalItemID   int64  // Radarr movieId
    ArrInstanceID    int64
    InstanceName     string
    Title            string
    OriginalTitle    string
    Year             int
    MediaFolderPath  string
    TargetFilePath   string
    HasFile          bool
    SizeOnDisk       int64  // 0 until the cache records size; see risk table
    Eligibility      string
    VideoCodec       string
    UpdatedAt        string
}

type SeriesRow struct {
    SeriesID        int64  // arr_library_cache.series_id (Sonarr seriesId)
    ArrInstanceID   int64
    InstanceName    string
    Title           string // series title, identical on every episode row
    Year            int
    MediaFolderPath string
    SeasonCount     int
    EpisodeCount    int
    EpisodeFileCount int
    Eligibility     string // worst verdict across the series: waiting_for_tags > missing_target > eligible
    UpdatedAt       string
}

type SeasonRow struct {
    SeasonNumber     int
    EpisodeCount     int
    EpisodeFileCount int
    PercentEpisodes  float64 // 100 * file/total, 0 when total == 0
}

type EpisodeRow struct {
    ID             int64
    SeriesID       int64
    ExternalItemID int64 // Sonarr episodeId
    SeasonNumber   int
    EpisodeNumber  int
    EpisodeTitle   string
    TargetFilePath string
    HasFile        bool
    Eligibility    string
    UpdatedAt      string
}

// ListMovies returns one page plus the unpaged total for the same filters.
func (s *Store) ListMovies(ctx context.Context, f HierarchyFilter) (int64, []MovieRow, error)

// ListSeries returns one page of series, aggregated in SQL (GROUP BY series_id),
// never by loading episodes into Go.
func (s *Store) ListSeries(ctx context.Context, f HierarchyFilter) (int64, []SeriesRow, error)

// SeriesByID is the header for the season view; found=false when absent.
func (s *Store) SeriesByID(ctx context.Context, seriesID int64) (SeriesRow, bool, error)

// SeasonsForSeries aggregates one series' seasons in SQL.
func (s *Store) SeasonsForSeries(ctx context.Context, seriesID int64) ([]SeasonRow, error)

// EpisodesForSeason returns one season, ordered by episode number. It is capped
// at LibrarySeasonMaxRows; when the cap bites the handler reports truncated.
func (s *Store) EpisodesForSeason(ctx context.Context, seriesID int64, season int) ([]EpisodeRow, error)
```

Constants (in the same file, beside the existing `LibraryPageDefaultLimit` /
`LibraryPageMaxLimit` in `library_read.go`, which are reused, not duplicated):

```go
// LibrarySeasonMaxRows is the hard cap for one season payload. A season above
// it is a data error, not a paging problem: report truncated instead of
// silently dropping episodes.
const LibrarySeasonMaxRows = 2000
```

### HTTP contract — `backend/internal/library/library.go`

Routes are added next to the existing four (which stay, deprecated, for one
release):

| method + route | query / body | 200 response |
|---|---|---|
| `GET /api/library/movies` | `q, instanceId, page, pageSize, sort, dir` | `{page, pageSize, total, items: MovieResource[]}` |
| `GET /api/library/series` | same | `{page, pageSize, total, items: SeriesResource[]}` |
| `GET /api/library/series/{seriesID}` | — | `SeriesResource` |
| `GET /api/library/series/{seriesID}/seasons` | — | `{items: SeasonResource[]}` |
| `GET /api/library/series/{seriesID}/seasons/{season}/episodes` | `page, pageSize` (default 200, max 500) | `{seasonNumber, total, truncated, items: EpisodeResource[]}` |

Errors, all JSON `{error, code}`:

- `400 invalid_page` — `page < 1`, `pageSize < 1`, non-numeric.
- `400 invalid_sort` — sort/dir outside the documented set.
- `404 not_found` — unknown series id, or `season` with no rows and no series.
- `503 authorization is not configured` — unchanged admin wrapper.

Resource JSON (additive to the existing item shape, same field names where they
overlap so the UI's types do not fork):

```json
// MovieResource
{"id":41,"arr_id":12,"instance_id":1,"instance":"Radarr","title":"Spirited Away",
 "original_title":"千と千尋の神隠し","year":2001,"path":"/media/movies/Spirited Away (2001)",
 "target_file_path":"…/Spirited Away (2001) [ArcA310].mkv","has_file":true,"size_on_disk":0,
 "eligibility":"eligible","video_codec":"hevc","updated_at":"…"}

// SeriesResource
{"id":7,"instance_id":2,"instance":"Sonarr","title":"The Office","year":2005,
 "path":"/media/tv/The Office (2005)","season_count":9,"episode_count":201,
 "episode_file_count":188,"eligibility":"waiting_for_tags","updated_at":"…"}

// SeasonResource
{"season_number":2,"statistics":{"episode_count":22,"episode_file_count":22,"percent_of_episodes":100.0}}

// EpisodeResource
{"id":913,"series_id":7,"arr_id":318,"season_number":2,"episode_number":3,
 "title":"Office Olympics","has_file":true,
 "target_file_path":"…/Season 02/The Office - S02E03.mkv","eligibility":"eligible"}
```

`size_on_disk` is part of the shape because Radarr's resource has it, but the
mirror does not store it; it ships as `0` and the UI hides the column until a
future migration adds it. Do not compute it with a `stat` per row — that is N
syscalls per page and lies under concurrent writes.

### SQL shape (why 12k stays fast)

```sql
-- movies: one page + count, both filtered, index-backed
SELECT … FROM arr_library_cache
 WHERE media_type = 'movie'
   AND (? = '' OR title LIKE ? COLLATE NOCASE OR original_title LIKE ? COLLATE NOCASE)
 ORDER BY title COLLATE NOCASE, id LIMIT ? OFFSET ?;

SELECT COUNT(*) FROM arr_library_cache WHERE media_type = 'movie' AND …;

-- series: aggregate once, page the aggregate
SELECT series_id, MIN(title), MIN(year), MIN(media_folder_path),
       COUNT(DISTINCT season_number) AS seasons,
       COUNT(*) AS episodes,
       SUM(target_file_path IS NOT NULL AND target_file_path <> '') AS files,
       MIN(CASE eligibility_status WHEN 'waiting_for_tags' THEN 1
                                   WHEN 'missing_target' THEN 2 ELSE 3 END) AS worst
  FROM arr_library_cache
 WHERE media_type = 'episode' AND series_id IS NOT NULL
 GROUP BY series_id
HAVING (? = '' OR MIN(title) LIKE ? COLLATE NOCASE)
 ORDER BY MIN(title) COLLATE NOCASE LIMIT ? OFFSET ?;

-- seasons: one query per series, GROUP BY season_number; episodes: one page.
```

The series `Total` counts the grouped rows, so it is computed as
`SELECT COUNT(*) FROM (… GROUP BY series_id HAVING …)` — one extra aggregate,
not a second full read into Go.

### Migration 0009 (new; indexes only)

```sql
-- 0009_library_hierarchy_indexes.sql
CREATE INDEX IF NOT EXISTS idx_arr_cache_series_tree
    ON arr_library_cache(series_id, season_number, episode_number);
CREATE INDEX IF NOT EXISTS idx_arr_cache_media_title
    ON arr_library_cache(media_type, title COLLATE NOCASE, id);
CREATE INDEX IF NOT EXISTS idx_arr_cache_media_updated
    ON arr_library_cache(media_type, updated_at DESC);
```

No table rebuild, no column change, no data migration. `schema.sql` is not
edited (repo convention: it mirrors 0001; forward migrations are authoritative).

### Frontend

`frontend/src/pages/Library.tsx` becomes a two-tab shell; the existing
per-row rescan action and the arr health strip are preserved.

- `frontend/src/components/library/MoviesTab.tsx` (new) — table, server-paged,
  search box debounced 250 ms, page-size selector capped at 200.
- `frontend/src/components/library/SeriesTab.tsx` (new) — series table; a row
  expands into seasons (fetched on expand, cached per series in component
  state), a season expands into its episode list (fetched on expand).
- `frontend/src/components/library/EligibilityBadge.tsx` (new) — one badge
  component for the three verdicts, reused by both tabs.
- `frontend/src/lib/api.ts` — typed wrappers for the five routes.

No client-side grouping, sorting or filtering of full datasets: every control
maps to a query parameter. Expansion state is UI-only and never triggers a
write.

### UI strings (D11 — labels in plain language, one tooltip per control)

| control | label | tooltip (what / when / cost) |
|---|---|---|
| tabs | “Movies” / “TV shows” | — (tabs, not options) |
| search | “Search titles” | “Filters on the server, across every title in the library. It matches part of a name, so ‘offi’ finds The Office. Cost: none, results update as you type.” |
| page size | “Rows per page” | “How many titles one page loads. Larger pages mean fewer clicks and a heavier first load. The maximum is 200.” |
| season row | “Season 2 — 22 of 22 episodes” | “Episodes with a file that Dubarr can see. Open the season to see each episode.” |

Empty states: “No movies match this search.” / “No TV shows match this
search.” Series with zero episodes: “This series has no episodes in the
library cache yet.”

## File-by-file changes

| file | change |
|---|---|
| `backend/internal/store/migrations/0009_library_hierarchy_indexes.sql` | **new** — three indexes |
| `backend/internal/store/library_hierarchy.go` | **new** — types + five read methods, `LibrarySeasonMaxRows` |
| `backend/internal/store/library_hierarchy_test.go` | **new** — query/aggregate/paging tests |
| `backend/internal/store/library_read.go` | no logic change; `ListLibrary` kept for `/api/library/items` |
| `backend/internal/library/library.go` | five handlers, route registration beside lines 84-87, `parseHierarchyFilter`, error codes |
| `backend/internal/library/hierarchy_test.go` | **new** — HTTP contract tests, 12k benchmark |
| `frontend/src/pages/Library.tsx` | two-tab shell, delegates to the two tab components |
| `frontend/src/components/library/MoviesTab.tsx` | **new** |
| `frontend/src/components/library/SeriesTab.tsx` | **new** |
| `frontend/src/components/library/EligibilityBadge.tsx` | **new** |
| `frontend/src/lib/api.ts` | five typed calls |

## Test plan

Store:
- `TestListMoviesPaginatesAndCountsWholeSet` — 250 movies, `pageSize=50`: 5 pages, total 250, no id repeated across pages, deterministic order on ties.
- `TestListMoviesSearchMatchesTitleAndOriginalTitle` — case-insensitive substring, unicode original title.
- `TestListSeriesGroupsEpisodeRowsIntoOneRow` — 3 series × 10 episodes → 3 rows, `episode_count=10`, `season_count` correct.
- `TestListSeriesAggregatesEpisodeFileCount` — mix of `target_file_path` null/empty/real.
- `TestListSeriesEligibilityIsWorstVerdict` — one `waiting_for_tags` episode makes the series `waiting_for_tags`.
- `TestSeasonsForSeriesAggregatesAndOrders` — seasons 1,2,10 ordered numerically, not lexically.
- `TestEpisodesForSeasonExcludesOtherSeasonsAndSeries` — the classic cross-series leak.
- `TestEpisodesForSeasonOrdersByEpisodeNumber`.
- `TestHierarchyFiltersRespectInstanceID`.
- `TestLibrarySeasonMaxRowsSetsTruncated` — 2001 synthetic episodes → 2000 returned, `truncated:true`.
- `TestHierarchyReadsDoNotWriteArrMirror` — snapshot `updated_at` + row count, run all five reads, assert byte-identical DB file hash (or `PRAGMA data_version` unchanged).

HTTP:
- `TestMoviesRouteValidatesPaging` — `page=0`, `pageSize=0`, `pageSize=501`, `sort=bogus` → 400 with documented codes.
- `TestMoviesRouteClampsPageSize` — `pageSize=100000` → 200 with `pageSize=200`.
- `TestSeriesRouteReturnsNestedStatistics` — JSON shape matches the contract above.
- `TestSeasonEpisodesRouteUnknownSeriesReturns404`.
- `TestLegacyItemsRouteStillServesFlatShape` — the old route is unchanged byte-for-byte.
- `BenchmarkListSeries12k` / `TestLibraryHierarchy12kUnder100ms` — seed 12 000 episode rows across 400 series, assert p95 < 100 ms locally (`-short` skips); this is the acceptance guard for “must stay fast”.

Frontend (if the repo's component tests run): `TestLibraryTabsFetchOnExpand` —
expanding a series issues exactly one seasons request and no episode request.

## Acceptance criteria

1. Sonarr data renders as series → seasons → episodes with one HTTP request per level; no response contains more than one series' episodes except the episodes endpoint.
2. `GET /api/library/series` over 12 000 episode rows returns in < 100 ms on the reference machine (benchmark committed and run in CI with a generous bound).
3. `pageSize` is clamped to 200 (movies/series) and 500 (episodes); out-of-range values never 500.
4. Season statistics are computed by SQL (`GROUP BY season_number`), provable by reading `library_hierarchy.go` — no `for` loop over episodes in Go for aggregation.
5. The five read handlers contain no `INSERT`, `UPDATE`, `DELETE` or `Exec`; a grep of the file finds only `Query*`.
6. `/api/library/items` still returns the pre-#56 shape (compat test).
7. 0009 is the only new migration; 0001–0008 untouched.
8. `go test ./...`, `go vet`, frontend typecheck pass.

## Migration and compatibility

- New migration **0009**, indexes only. `CREATE INDEX` on 12k rows is instant; on a large cache it is a one-off at boot.
- API: five new routes; the four old ones stay. No field removed or renamed; `LibraryListItem` JSON is unchanged.
- `id` in movie/episode resources is `arr_library_cache.id` (what `/rescan` already takes). `arr_id` is the arr's own id — never conflate them.
- Ordering is deterministic (`title, id`) so paging is stable while `arrsync` rewrites the snapshot. A row inserted mid-pagination can still shift one page; accepted, documented in the UI as normal refresh behaviour.

## Risks and mitigations

| risk | mitigation |
|---|---|
| Substring `LIKE '%q%'` cannot use the index | 12k rows makes a scan cheap; benchmark pins the budget. If it ever bites, the next step is an FTS5 table fed by `arrsync`, not client-side filtering |
| Series title taken from `MIN(title)` when rows disagree | Test asserts all rows of a series share `title`; if a real instance disagrees, the series detail shows the majority title and the mismatch is logged once per pass |
| Grouping hides a series whose episodes were all removed | `arrsync` deletes rows (snapshot rewrite); the series disappears by design — document, do not invent tombstone tables |
| Season payload too large for a pathological anime season | `LibrarySeasonMaxRows` + `truncated` flag, never silent truncation |
| N+1 between UI expansion and requests | Seasons/episodes fetched lazily and cached per series in component state; a series list page issues zero extra requests |
| `size_on_disk` shown as 0 confusing users | Column hidden while 0; noted as out of scope rather than faked |

## Deliberately not adopted from arr

`monitored` flags and season monitoring, quality profiles and cutoff,
`images`/`ratings`/`overview`/`genres`, calendar and wanted/missing, season
pass, arr's rename/move endpoints, import lists, hardlinks, recycle bin. The
library view is a **read mirror**; editing arr state stays in the arr.

## Do not touch

`AGENTS.md`, `PLAN.md`, `GLOSSARY.md`, `docs/adr/**`, `LICENSE`, migrations
0001–0008, `backend/schema.sql`, `backend/internal/arrsync/*` (the writer of the
mirror), `backend/internal/store/arr_library_sync.go`, `library_read.go`'s
existing `ListLibrary` behaviour, the Processing Gate columns.
