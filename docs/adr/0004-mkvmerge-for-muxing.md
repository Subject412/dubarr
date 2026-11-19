# mkvmerge for MKV muxing, ffmpeg for extraction and DSP

Final muxing uses mkvmerge (--language/--sync, atomic rename after ffprobe validation) while ffmpeg handles lossless extraction, windowing and tempo adjustment. We add the MKVToolNix dependency because ffmpeg's Matroska muxer gives weaker control over track language/title tags and default-track flags than mkvmerge.

Status: accepted
