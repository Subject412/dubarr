-- Which analysis mode a job actually used, and why a requested windowed run did
-- not run as windows. Both are nullable and unbackfilled on purpose: a row
-- analysed before windowed analysis existed has no mode to record, and a
-- backfill would be a guess about a decode that is gone. The API returns empty
-- for NULL, which the UI renders as "—".
ALTER TABLE sync_mux_jobs ADD COLUMN analysis_source TEXT;
ALTER TABLE sync_mux_jobs ADD COLUMN analysis_fallback TEXT;
