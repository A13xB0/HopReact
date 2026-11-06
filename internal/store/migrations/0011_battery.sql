-- Low battery alerts.
--
-- A battery rule lives in its own table rather than in watch_rules. It judges
-- a voltage, not a silence, so it shares none of watch_rules' columns, and
-- widening watch_rules' source CHECK would mean rebuilding that table, which
-- with foreign keys on would cascade-delete every rule's alert state.
--
-- One battery rule per watch. Its alert state sits on the same row.
CREATE TABLE battery_rules (
  watch_id        INTEGER PRIMARY KEY REFERENCES watches(id) ON DELETE CASCADE,
  -- The owner's own levels in millivolts. NULL follows the site's level, so
  -- a site change reaches everyone who never chose their own.
  low_mv          INTEGER,
  critical_mv     INTEGER,
  -- The overcharge alert is off until its owner turns it on.
  overcharge_on   INTEGER NOT NULL DEFAULT 0,
  overcharge_mv   INTEGER,
  created_at      INTEGER NOT NULL,
  level           TEXT    NOT NULL DEFAULT 'unknown'
                    CHECK (level IN ('unknown','ok','low','critical','overcharged')),
  since           INTEGER NOT NULL,
  candidate       TEXT    NOT NULL DEFAULT '',
  candidate_count INTEGER NOT NULL DEFAULT 0,
  last_reading_at INTEGER,
  last_mv         INTEGER,
  -- The most serious level announced this episode; '' when nobody was told,
  -- which also keeps the "OK again" message quiet.
  announced       TEXT    NOT NULL DEFAULT ''
);

-- When this watch was given a battery rule, or chose not to have one. Set
-- once and never cleared, so a battery rule someone removed is never added
-- back by itself.
ALTER TABLE watches ADD COLUMN battery_added_at INTEGER;

-- Whether HopReact adds battery rules to this person's repeaters by itself.
ALTER TABLE users ADD COLUMN auto_battery INTEGER NOT NULL DEFAULT 1;
-- How many repeaters the last "battery alerts are on" message covered, for
-- the one-off banner on the watches page. 0 once dismissed.
ALTER TABLE users ADD COLUMN battery_banner INTEGER NOT NULL DEFAULT 0;

-- The latest battery readings CoreScope has for each watched repeater, so the
-- pages can show a voltage and a week's trend without asking CoreScope on
-- every page load.
CREATE TABLE battery_readings (
  key          TEXT    PRIMARY KEY,
  status       TEXT    NOT NULL,
  latest_mv    INTEGER NOT NULL DEFAULT 0,
  latest_at    INTEGER,
  reporter     TEXT    NOT NULL DEFAULT '',
  stale        INTEGER NOT NULL DEFAULT 0,
  cs_low_mv    INTEGER NOT NULL DEFAULT 0,
  cs_critical_mv INTEGER NOT NULL DEFAULT 0,
  -- JSON [[unix, mv], ...], oldest first, at most a week.
  samples      TEXT    NOT NULL DEFAULT '[]',
  fetched_at   INTEGER NOT NULL
);
