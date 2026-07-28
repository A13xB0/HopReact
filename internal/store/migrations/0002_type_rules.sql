-- Per-payload-type alert rules.
--
-- Before this, a watch had exactly two possible signals: 'seen' (CoreScope's
-- last_seen — heard at all) and, opt-in, 'relayed' (its last_relayed — seen in
-- any packet route). Both are single dials over all traffic at once, so a
-- repeater that still adverts but has stopped carrying messages looks fine.
--
-- A watch now owns any number of rules, each with its own threshold. Rules are
-- ORed: any one of them going over produces an alert, and the drainer
-- coalesces a poll's worth into a single message per watch.

CREATE TABLE watch_rules (
  id              INTEGER PRIMARY KEY,
  watch_id        INTEGER NOT NULL REFERENCES watches(id) ON DELETE CASCADE,
  label           TEXT    NOT NULL DEFAULT '',
  -- Where the rule's timestamp comes from. The two feed sources are
  -- CoreScope's own aggregates and stay exactly as they always were:
  --   'seen'    → targets.last_seen_at
  --   'relayed' → targets.last_relayed_at
  -- 'types' is the new one, reading per-type evidence HopReact attributes
  -- itself from the packet feed.
  --
  -- Keeping the feed sources rather than re-expressing them as "all types"
  -- is deliberate. Our per-type evidence is a strict SUBSET of what CoreScope
  -- sees: we only accept path hops three bytes wide or more, which is about
  -- 41% of packets. Rebuilding 'seen' on top of that would quietly tighten
  -- every existing user's alerting and start manufacturing outages.
  source          TEXT    NOT NULL CHECK (source IN ('seen','relayed','types')),
  -- Comma-separated payload type integers, for source='types'. Groups are
  -- expanded to their members before being stored, so redefining a group
  -- later cannot silently change what an existing rule alerts on.
  types           TEXT    NOT NULL DEFAULT '',
  -- 'sent' | 'carried' | 'either', for source='types'. Only adverts can ever
  -- produce 'sent' evidence; see internal/attribute.
  direction       TEXT    NOT NULL DEFAULT 'either'
                    CHECK (direction IN ('sent','carried','either')),
  threshold_hours INTEGER NOT NULL CHECK (threshold_hours >= 1),
  created_at      INTEGER NOT NULL
);
CREATE INDEX watch_rules_watch ON watch_rules(watch_id);

-- Reproduce every existing watch's behaviour exactly: one rule for the
-- signal it always had, and a second only where the relay opt-in was set.
INSERT INTO watch_rules (watch_id, label, source, types, direction, threshold_hours, created_at)
SELECT id, 'Not heard at all', 'seen', '', 'either', threshold_hours, created_at
FROM watches;

INSERT INTO watch_rules (watch_id, label, source, types, direction, threshold_hours, created_at)
SELECT id, 'Stopped passing traffic', 'relayed', '', 'carried', threshold_hours, created_at
FROM watches WHERE alert_on_relay = 1;

-- When each payload type was last seen for each target, and in which
-- capacity. Last-seen only — no time series, so this stays small (at most
-- one row per node per type per direction).
--
-- Recorded for every attributable node, not just watched ones, so a new watch
-- starts with real history instead of being useless for its first few hours.
CREATE TABLE target_activity (
  kind           TEXT    NOT NULL,
  key            TEXT    NOT NULL,
  payload_type   INTEGER NOT NULL,
  direction      TEXT    NOT NULL CHECK (direction IN ('sent','carried')),
  last_at        INTEGER NOT NULL,
  -- How many packets have ever supported this row. Surfaced in the UI: a
  -- rule resting on two sightings deserves to be labelled as thin rather
  -- than quietly trusted.
  evidence_count INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (kind, key, payload_type, direction)
);

-- How far through the packet feed we have read, so re-polling an overlapping
-- window doesn't double-count evidence.
CREATE TABLE feed_cursor (
  id             INTEGER PRIMARY KEY CHECK (id = 1),
  last_packet_id INTEGER NOT NULL DEFAULT 0,
  updated_at     INTEGER NOT NULL DEFAULT 0
);
INSERT INTO feed_cursor (id, last_packet_id, updated_at) VALUES (1, 0, 0);

-- watch_state is re-keyed from (watch_id, signal) to (watch_id, rule_id).
-- SQLite cannot change a primary key in place, so this is the standard
-- rebuild. The join carries every existing state row onto its equivalent new
-- rule, which keeps alert history — and, more importantly, notify_count —
-- intact across the upgrade. Losing notify_count would let a watch that is
-- already alerting announce itself a second time.
CREATE TABLE watch_state_new (
  watch_id         INTEGER NOT NULL REFERENCES watches(id) ON DELETE CASCADE,
  rule_id          INTEGER NOT NULL REFERENCES watch_rules(id) ON DELETE CASCADE,
  state            TEXT    NOT NULL CHECK (state IN ('unknown','ok','pending','alerting','recovering')),
  since            INTEGER NOT NULL,
  consecutive      INTEGER NOT NULL DEFAULT 0,
  observed_at      INTEGER,
  alerting_since   INTEGER,
  last_notified_at INTEGER,
  notify_count     INTEGER NOT NULL DEFAULT 0,
  seeded           INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (watch_id, rule_id)
);

INSERT INTO watch_state_new (watch_id, rule_id, state, since, consecutive,
    observed_at, alerting_since, last_notified_at, notify_count, seeded)
SELECT ws.watch_id, r.id, ws.state, ws.since, ws.consecutive,
       ws.observed_at, ws.alerting_since, ws.last_notified_at,
       ws.notify_count, ws.seeded
FROM watch_state ws
JOIN watch_rules r ON r.watch_id = ws.watch_id AND r.source = ws.signal;

DROP TABLE watch_state;
ALTER TABLE watch_state_new RENAME TO watch_state;

-- alert_events keeps its append-only history. Its signal column becomes a
-- rule id for new rows; old rows keep their 'seen'/'relayed' text, which is
-- still the honest record of what was decided at the time.
ALTER TABLE alert_events ADD COLUMN rule_id INTEGER;
