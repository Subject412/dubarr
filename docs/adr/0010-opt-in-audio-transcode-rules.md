# Opt-in audio transcode rules override the no-lossy-re-encode default

Users may define ordered Transcode Rules that change the codec, channels, bitrate or sample rate of the track injected into the Target MKV. Rules are evaluated against the temp-folder copy of the vault track before muxing, first match wins, and an empty rule list preserves the lossless-copy behaviour.

This deliberately overrides the original spec's "no lossy re-encode except for FPS/tempo" boundary as an explicit user opt-in; the Audio Vault stays write-once and lossless (ADR-0002). We accept irreversible quality loss in the deliverable in exchange for a predictable output format, and fold tempo adjustment and transcoding into a single ffmpeg encode to avoid a second generation loss. The spec and glossary are updated in the same change so the default remains "no transcoding".

Status: accepted
