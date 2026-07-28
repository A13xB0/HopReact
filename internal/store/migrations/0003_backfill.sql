-- One-off history backfill.
--
-- Per-type evidence previously only accumulated from the moment the feature
-- started running, so a fresh install showed "never" against every payload
-- type until a day's traffic had trickled through — and a rule with no
-- evidence deliberately cannot fire, so the tool was quietly less useful than
-- it looked for its first day.
--
-- The fix is to read a window of history once, at startup. CoreScope will do
-- the windowing given ?since=, so 24 hours costs a single request: about 4,600
-- packets and under five megabytes on the live mesh.

-- The oldest packet id already consumed. Together with last_packet_id this
-- gives the half-open range that has been counted, so the backfill can ingest
-- strictly older packets without double-counting the overlap. Without it,
-- every backfilled packet inside the already-read window would be added to
-- evidence_count a second time.
ALTER TABLE feed_cursor ADD COLUMN low_packet_id INTEGER NOT NULL DEFAULT 0;

-- When the backfill last ran. Zero means never, which is what triggers it.
ALTER TABLE feed_cursor ADD COLUMN backfilled_at INTEGER NOT NULL DEFAULT 0;
