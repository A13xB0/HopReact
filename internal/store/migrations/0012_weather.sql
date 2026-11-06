-- Hourly weather per 0.1° grid square, for battery analysis.
--
-- Keyed by the square, never by a repeater, so the cache holds no exact
-- positions and repeaters near each other share rows.
CREATE TABLE weather_hours (
  cell      TEXT    NOT NULL,
  at        INTEGER NOT NULL, -- end of the hour, unix seconds
  temp_c    REAL    NOT NULL,
  cloud_pct REAL    NOT NULL,
  sun_wm2   REAL    NOT NULL,
  PRIMARY KEY (cell, at)
) WITHOUT ROWID;

CREATE TABLE weather_fetches (
  cell       TEXT    PRIMARY KEY,
  fetched_at INTEGER NOT NULL,
  past_days  INTEGER NOT NULL
);
