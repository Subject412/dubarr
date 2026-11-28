package arr

// Kind identifies which *arr application a client talks to.
type Kind string

const (
	KindRadarr Kind = "radarr"
	KindSonarr Kind = "sonarr"
)

// Language is an *arr language reference (tmdb/tvdb id plus display name).
type Language struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// Tag is one *arr tag from GET /api/v3/tag (Radarr) or GET /api/v5/tag
// (Sonarr). Both products expose the same shape, and ADR-0009 matches
// eligibility by Label. IDs are not stable across instances, so they only ever
// identify a tag within its own instance.
type Tag struct {
	ID    int64  `json:"id"`
	Label string `json:"label"`
}

// Movie is the subset of a Radarr /api/v3/movie record Dubarr needs.
type Movie struct {
	ID    int    `json:"id"`
	Title string `json:"title"`
	// OriginalTitle is arr's originalTitle, empty when the instance omits it.
	OriginalTitle    string   `json:"originalTitle"`
	Year             int      `json:"year"`
	TmdbID           int      `json:"tmdbId"`
	ImdbID           string   `json:"imdbId"`
	Path             string   `json:"path"`
	FolderName       string   `json:"folderName"`
	QualityProfileID int      `json:"qualityProfileId"`
	Monitored        bool     `json:"monitored"`
	HasFile          bool     `json:"hasFile"`
	OriginalLanguage Language `json:"originalLanguage"`
	// Tags are the tag ids attached to the movie; resolve them with Tags().
	Tags []int `json:"tags"`
	// MovieFile is the imported file, nil when HasFile is false.
	MovieFile *MediaFile `json:"movieFile"`
}

// Series is the subset of a Sonarr /api/v5/series record Dubarr needs.
type Series struct {
	ID    int    `json:"id"`
	Title string `json:"title"`
	// OriginalTitle is arr's originalTitle, empty when the instance omits it.
	OriginalTitle    string `json:"originalTitle"`
	Year             int    `json:"year"`
	TvdbID           int    `json:"tvdbId"`
	ImdbID           string `json:"imdbId"`
	Path             string `json:"path"`
	FolderName       string `json:"folderName"`
	QualityProfileID int    `json:"qualityProfileId"`
	Monitored        bool   `json:"monitored"`
	// OriginalLanguage is the series' original audio language (the v5 series
	// object returns it); empty when the instance omits it.
	OriginalLanguage Language `json:"originalLanguage"`
	// Tags are the tag ids attached to the series; episodes inherit them.
	Tags []int `json:"tags"`
}

// QualityProfile is a named quality profile from {apiBase}/qualityprofile.
type QualityProfile struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// RootFolder is a configured media root from {apiBase}/rootfolder.
type RootFolder struct {
	ID         int    `json:"id"`
	Path       string `json:"path"`
	Accessible bool   `json:"accessible"`
	FreeSpace  int64  `json:"freeSpace"`
}
