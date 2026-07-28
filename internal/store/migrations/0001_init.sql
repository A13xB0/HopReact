-- Initial schema.
--
-- Every timestamp is INTEGER unix seconds, not TEXT. Thresholds are
-- compared and sorted on these constantly, and RFC3339 strings only exist
-- at the CoreScope and display boundaries.

-- A person, identified only by Discord. No email, no password: Discord does
-- both sign-in and delivery, so there is no personal data here beyond an
-- account id and whatever display name Discord reports.
CREATE TABLE users (
  id              INTEGER PRIMARY KEY,
  discord_id      TEXT    NOT NULL UNIQUE,
  username        TEXT    NOT NULL,
  avatar          TEXT,
  created_at      INTEGER NOT NULL,
  last_login_at   INTEGER NOT NULL,
  -- Delivery health. A bot can only DM someone it shares a server with, so
  -- leaving HopReact's Discord (or disabling DMs from server members)
  -- silently disables every alert this person relies on. Recorded here so
  -- the dashboard can say so instead of failing quietly.
  dm_ok            INTEGER NOT NULL DEFAULT 1,
  dm_failed_reason TEXT,
  dm_checked_at    INTEGER
);

-- Opaque server-side sessions. Only the SHA-256 of the cookie value is
-- stored, so a database leak yields no usable sessions.
CREATE TABLE sessions (
  token_hash  BLOB    PRIMARY KEY,
  user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  csrf_token  TEXT    NOT NULL,
  created_at  INTEGER NOT NULL,
  expires_at  INTEGER NOT NULL
);
CREATE INDEX sessions_expires ON sessions(expires_at);
CREATE INDEX sessions_user ON sessions(user_id);

-- What CoreScope last told us about each watchable thing. Nodes and
-- observers share this table; (kind, key) keeps them apart even when they
-- are the same physical box, which 7 of 9 observers on the live instance
-- are.
CREATE TABLE targets (
  kind        TEXT    NOT NULL CHECK (kind IN ('node','observer')),
  key         TEXT    NOT NULL,
  name        TEXT    NOT NULL DEFAULT '',
  role        TEXT    NOT NULL DEFAULT '',
  -- Monotonic: only ever moved forward (see store.UpsertTargets). A stale
  -- or partial upstream view must not be able to drag a target backwards
  -- into a false alert.
  last_seen_at    INTEGER,
  last_relayed_at INTEGER,
  -- NULL until this target has been observed relaying even once. 189 nodes
  -- on the live instance have never relayed; without this the relay alert
  -- would fire for nodes that never had the chance to stop.
  relay_ever_observed_at INTEGER,
  relay_count_1h  INTEGER NOT NULL DEFAULT 0,
  relay_count_24h INTEGER NOT NULL DEFAULT 0,
  lat REAL, lon REAL,
  first_indexed_at INTEGER NOT NULL,
  last_in_feed_at  INTEGER NOT NULL,
  updated_at       INTEGER NOT NULL,
  PRIMARY KEY (kind, key)
);
CREATE INDEX targets_name ON targets(name);

-- A subscription, not ownership: several people may watch the same target
-- with different thresholds, so there is no land-grab and no approval
-- queue.
CREATE TABLE watches (
  id              INTEGER PRIMARY KEY,
  user_id         INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  target_kind     TEXT    NOT NULL CHECK (target_kind IN ('node','observer')),
  target_key      TEXT    NOT NULL,
  threshold_hours INTEGER NOT NULL CHECK (threshold_hours >= 1),
  -- Opt-in, never the default: plenty of nodes have never relayed at all.
  alert_on_relay  INTEGER NOT NULL DEFAULT 0,
  label           TEXT,
  muted_until     INTEGER,
  created_at      INTEGER NOT NULL,
  UNIQUE (user_id, target_kind, target_key)
);
CREATE INDEX watches_user ON watches(user_id);
CREATE INDEX watches_target ON watches(target_kind, target_key);

-- Alert state, per watch AND per signal. A node can be heard while having
-- stopped relaying; each signal needs its own hysteresis and its own
-- notification history.
CREATE TABLE watch_state (
  watch_id        INTEGER NOT NULL REFERENCES watches(id) ON DELETE CASCADE,
  signal          TEXT    NOT NULL CHECK (signal IN ('seen','relayed')),
  state           TEXT    NOT NULL CHECK (state IN ('unknown','ok','pending','alerting','recovering')),
  since           INTEGER NOT NULL,
  consecutive     INTEGER NOT NULL DEFAULT 0,
  observed_at     INTEGER,
  alerting_since  INTEGER,
  last_notified_at INTEGER,
  -- 0 means no alert was ever announced for the current episode, which
  -- suppresses the recovery message too — you never announce recovery from
  -- an alert nobody was told about.
  notify_count    INTEGER NOT NULL DEFAULT 0,
  -- Set when the watch was created against an already-offline target and
  -- seeded straight to 'alerting' without notifying.
  seeded          INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (watch_id, signal)
);

-- One row per poll, whether or not it was believed. This is the audit trail
-- for "why did/didn't I get an alert", and the source of the dashboard's
-- upstream-health banner.
CREATE TABLE poll_runs (
  id                INTEGER PRIMARY KEY,
  started_at        INTEGER NOT NULL,
  finished_at       INTEGER,
  status            TEXT    NOT NULL CHECK (status IN ('ok','failed','suspect')),
  node_count        INTEGER NOT NULL DEFAULT 0,
  observer_count    INTEGER NOT NULL DEFAULT 0,
  advanced_count    INTEGER NOT NULL DEFAULT 0,
  evaluated         INTEGER NOT NULL DEFAULT 0,
  suppressed_reason TEXT,
  error             TEXT
);
CREATE INDEX poll_runs_started ON poll_runs(started_at);

-- Outbox. Transitions write here inside the same transaction that commits
-- the state change; a drainer sends afterwards. Committing state first
-- means a crash loses at most one message rather than re-sending a storm.
CREATE TABLE notifications (
  id         INTEGER PRIMARY KEY,
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  kind       TEXT    NOT NULL,
  payload    TEXT    NOT NULL,
  created_at INTEGER NOT NULL,
  send_after INTEGER NOT NULL,
  attempts   INTEGER NOT NULL DEFAULT 0,
  sent_at    INTEGER,
  last_error TEXT
);
CREATE INDEX notifications_pending ON notifications(sent_at, send_after);

-- Append-only history of every state change. Earns its keep the first time
-- someone says "you never told me".
CREATE TABLE alert_events (
  id          INTEGER PRIMARY KEY,
  watch_id    INTEGER NOT NULL,
  signal      TEXT    NOT NULL,
  from_state  TEXT    NOT NULL,
  to_state    TEXT    NOT NULL,
  at          INTEGER NOT NULL,
  poll_run_id INTEGER,
  notified    INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX alert_events_watch ON alert_events(watch_id, at);
