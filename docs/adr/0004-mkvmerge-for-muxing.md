# mkvmerge for MKV muxing, ffmpeg for extraction and DSP

Final muxing uses mkvmerge (--language/--sync, atomic rename after ffprobe validation) while ffmpeg handles lossless extraction, windowing and tempo adjustment. We add the MKVToolNix dependency because ffmpeg's Matroska muxer gives weaker control over track language/title tags and default-track flags than mkvmerge. The appended track is named for its language (`Polish`), derived from the tag the job already targets, never by a configured string — the `mux.options` track-title setting was retired in 0.6.0. A target language that cannot be resolved to an ISO 639-2 code is written as `und` with the legacy `ZZ DUB` title rather than failing the mux, because mkvmerge refuses an invalid `--language` and a file with an honest unknown language beats no file.

Status: accepted
