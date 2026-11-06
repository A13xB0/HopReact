package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"hopreact/internal/weather"
)

// weatherKeep is how long cached weather is kept: a little past the longest
// range the analysis page shows, and Open-Meteo's own history.
const weatherKeep = 100 * 24 * time.Hour

// WeatherHours returns a grid square's cached hours between from and to.
func (s *Store) WeatherHours(ctx context.Context, cell string, from, to time.Time) ([]weather.Hour, error) {
	rows, err := s.read.QueryContext(ctx, `
		SELECT at, temp_c, cloud_pct, sun_wm2 FROM weather_hours
		WHERE cell = ? AND at >= ? AND at <= ? ORDER BY at`, cell, from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []weather.Hour
	for rows.Next() {
		var h weather.Hour
		var at int64
		if err := rows.Scan(&at, &h.TempC, &h.CloudPct, &h.SunWm2); err != nil {
			return nil, err
		}
		h.At = time.Unix(at, 0).UTC()
		out = append(out, h)
	}
	return out, rows.Err()
}

// SaveWeather replaces a square's hours from the first one given onwards,
// records the fetch, and drops hours older than weatherKeep.
func (s *Store) SaveWeather(ctx context.Context, cell string, hours []weather.Hour, fetchedAt time.Time, pastDays int) error {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if len(hours) > 0 {
		// Forecast hours that a newer run no longer covers would otherwise
		// linger as if still current.
		if _, err := tx.ExecContext(ctx, `DELETE FROM weather_hours WHERE cell = ? AND at >= ?`,
			cell, hours[0].At.Unix()); err != nil {
			return err
		}
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT OR REPLACE INTO weather_hours (cell, at, temp_c, cloud_pct, sun_wm2) VALUES (?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, h := range hours {
		if _, err := stmt.ExecContext(ctx, cell, h.At.Unix(), h.TempC, h.CloudPct, h.SunWm2); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO weather_fetches (cell, fetched_at, past_days) VALUES (?,?,?)
		ON CONFLICT(cell) DO UPDATE SET fetched_at = excluded.fetched_at, past_days = excluded.past_days`,
		cell, fetchedAt.Unix(), pastDays); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM weather_hours WHERE at < ?`,
		fetchedAt.Add(-weatherKeep).Unix()); err != nil {
		return err
	}
	return tx.Commit()
}

// WeatherFetched says when a square was last fetched and how far back.
func (s *Store) WeatherFetched(ctx context.Context, cell string) (time.Time, int, bool, error) {
	var at int64
	var past int
	err := s.read.QueryRowContext(ctx,
		`SELECT fetched_at, past_days FROM weather_fetches WHERE cell = ?`, cell).Scan(&at, &past)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, 0, false, nil
	}
	if err != nil {
		return time.Time{}, 0, false, err
	}
	return time.Unix(at, 0).UTC(), past, true, nil
}
