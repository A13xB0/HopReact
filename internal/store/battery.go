package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"hopreact/internal/corescope"
)

// BatteryRule is a watch's low battery rule and its alert state.
//
// The state fields mirror alert.BatteryState; they are plain strings here
// because the alert package already depends on this one.
type BatteryRule struct {
	WatchID int64
	// LowMV and CriticalMV are the owner's own levels; 0 follows the site.
	LowMV      int
	CriticalMV int
	// OverchargeOn turns the overcharge alert on, at OverchargeMV or the
	// site's level when that is 0.
	OverchargeOn bool
	OverchargeMV int
	CreatedAt    time.Time

	Level          string
	Since          time.Time
	Candidate      string
	CandidateCount int
	LastReadingAt  time.Time
	LastMV         int
	Announced      string
}

// BatteryReadings is the latest CoreScope battery report for one repeater.
type BatteryReadings struct {
	Key          string
	Status       corescope.BatteryStatus
	LatestMV     int
	LatestAt     time.Time
	Reporter     string
	Stale        bool
	CSLowMV      int
	CSCriticalMV int
	Samples      []corescope.BatterySample
	FetchedAt    time.Time
}

const batteryRuleCols = `watch_id, COALESCE(low_mv,0), COALESCE(critical_mv,0), overcharge_on,
	COALESCE(overcharge_mv,0), created_at, level, since, candidate, candidate_count,
	last_reading_at, COALESCE(last_mv,0), announced`

func scanBatteryRule(row interface{ Scan(...any) error }) (BatteryRule, error) {
	var r BatteryRule
	var over int
	var created, since int64
	var lastAt sql.NullInt64
	if err := row.Scan(&r.WatchID, &r.LowMV, &r.CriticalMV, &over, &r.OverchargeMV, &created,
		&r.Level, &since, &r.Candidate, &r.CandidateCount, &lastAt, &r.LastMV, &r.Announced); err != nil {
		return BatteryRule{}, err
	}
	r.OverchargeOn = over != 0
	r.CreatedAt = time.Unix(created, 0).UTC()
	r.Since = time.Unix(since, 0).UTC()
	r.LastReadingAt = timeFrom(lastAt)
	return r, nil
}

// BatteryRules returns every battery rule keyed by watch id.
func (s *Store) BatteryRules(ctx context.Context) (map[int64]BatteryRule, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT `+batteryRuleCols+` FROM battery_rules`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]BatteryRule{}
	for rows.Next() {
		r, err := scanBatteryRule(rows)
		if err != nil {
			return nil, err
		}
		out[r.WatchID] = r
	}
	return out, rows.Err()
}

// BatteryRulesForUser returns one person's battery rules keyed by watch id.
func (s *Store) BatteryRulesForUser(ctx context.Context, userID int64) (map[int64]BatteryRule, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT `+batteryRuleCols+` FROM battery_rules
		WHERE watch_id IN (SELECT id FROM watches WHERE user_id = ?)`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]BatteryRule{}
	for rows.Next() {
		r, err := scanBatteryRule(rows)
		if err != nil {
			return nil, err
		}
		out[r.WatchID] = r
	}
	return out, rows.Err()
}

// BatteryRuleFor returns one watch's battery rule, and whether it has one.
func (s *Store) BatteryRuleFor(ctx context.Context, watchID int64) (BatteryRule, bool, error) {
	r, err := scanBatteryRule(s.read.QueryRowContext(ctx,
		`SELECT `+batteryRuleCols+` FROM battery_rules WHERE watch_id = ?`, watchID))
	if errors.Is(err, sql.ErrNoRows) {
		return BatteryRule{}, false, nil
	}
	return r, err == nil, err
}

// InsertBatteryRuleTx adds a battery rule with its starting state and marks
// the watch as having been given one, so it is never added again by itself.
func (s *Store) InsertBatteryRuleTx(ctx context.Context, tx *sql.Tx, r BatteryRule, now time.Time) error {
	if r.Level == "" {
		r.Level = "unknown"
	}
	if r.Since.IsZero() {
		r.Since = now
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO battery_rules (watch_id, low_mv, critical_mv, overcharge_on, overcharge_mv,
			created_at, level, since, candidate, candidate_count, last_reading_at, last_mv, announced)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(watch_id) DO NOTHING`,
		r.WatchID, nullMV(r.LowMV), nullMV(r.CriticalMV), boolInt(r.OverchargeOn), nullMV(r.OverchargeMV),
		now.Unix(), r.Level, r.Since.Unix(), r.Candidate, r.CandidateCount,
		nullInt(r.LastReadingAt), nullMV(r.LastMV), r.Announced); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx,
		`UPDATE watches SET battery_added_at = COALESCE(battery_added_at, ?) WHERE id = ?`, now.Unix(), r.WatchID)
	return err
}

// SaveBatteryStateTx writes a battery rule's alert state, leaving its
// settings alone, but only if the stored state is still old. It reports
// whether it saved: false means the rule was removed or its owner reset it
// while the check ran, and their change stands.
func (s *Store) SaveBatteryStateTx(ctx context.Context, tx *sql.Tx, old, r BatteryRule) (bool, error) {
	res, err := tx.ExecContext(ctx, `
		UPDATE battery_rules SET level = ?, since = ?, candidate = ?, candidate_count = ?,
			last_reading_at = ?, last_mv = ?, announced = ?
		WHERE watch_id = ? AND level = ? AND since = ? AND candidate = ? AND candidate_count = ?
			AND COALESCE(last_reading_at, 0) = ? AND announced = ?`,
		r.Level, r.Since.Unix(), r.Candidate, r.CandidateCount,
		nullInt(r.LastReadingAt), nullMV(r.LastMV), r.Announced,
		r.WatchID, old.Level, old.Since.Unix(), old.Candidate, old.CandidateCount,
		unixOrZero(old.LastReadingAt), old.Announced)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// InsertAutoBatteryRuleTx adds a battery rule the poller decided on, but only
// if the watch has still never had its battery choice made and its owner
// still wants rules added automatically. It reports whether it added one.
func (s *Store) InsertAutoBatteryRuleTx(ctx context.Context, tx *sql.Tx, r BatteryRule, now time.Time) (bool, error) {
	var ok int
	err := tx.QueryRowContext(ctx, `
		SELECT 1 FROM watches w JOIN users u ON u.id = w.user_id
		WHERE w.id = ? AND w.battery_added_at IS NULL AND u.auto_battery = 1
			AND NOT EXISTS (SELECT 1 FROM battery_rules b WHERE b.watch_id = w.id)`, r.WatchID).Scan(&ok)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, s.InsertBatteryRuleTx(ctx, tx, r, now)
}

func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// SetBatteryBannerTx records how many repeaters a "battery alerts are on"
// message covered, for the one-off banner.
func (s *Store) SetBatteryBannerTx(ctx context.Context, tx *sql.Tx, userID int64, n int) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE users SET battery_banner = battery_banner + ? WHERE id = ?`, n, userID)
	return err
}

// AddBatteryRule turns battery alerts on for one of a user's watches.
func (s *Store) AddBatteryRule(ctx context.Context, userID, watchID int64) error {
	now := s.Now().UTC()
	return s.tx(ctx, func(tx *sql.Tx) error {
		if err := ownedTx(ctx, tx, userID, watchID); err != nil {
			return err
		}
		return s.InsertBatteryRuleTx(ctx, tx, BatteryRule{WatchID: watchID}, now)
	})
}

// DeleteBatteryRule turns battery alerts off for one watch. The watch keeps
// its battery_added_at, so the rule is never added back by itself.
func (s *Store) DeleteBatteryRule(ctx context.Context, userID, watchID int64) error {
	now := s.Now().UTC()
	return s.tx(ctx, func(tx *sql.Tx) error {
		if err := ownedTx(ctx, tx, userID, watchID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM battery_rules WHERE watch_id = ?`, watchID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`UPDATE watches SET battery_added_at = COALESCE(battery_added_at, ?) WHERE id = ?`, now.Unix(), watchID)
		return err
	})
}

// UpdateBatteryLevels changes a battery rule's levels, keeping its state.
// Zero means "follow the site". The next new reading is judged against the
// new levels through the usual two-reading confirmation.
//
// Turning the overcharge alert off while it is the current alert drops the
// rule to unknown rather than letting the next reading announce "OK again"
// for a problem that was switched off, not fixed.
func (s *Store) UpdateBatteryLevels(ctx context.Context, userID, watchID int64, lowMV, criticalMV int, overOn bool, overMV int) error {
	now := s.Now().UTC()
	return s.tx(ctx, func(tx *sql.Tx) error {
		if err := ownedTx(ctx, tx, userID, watchID); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `
			UPDATE battery_rules SET low_mv = ?, critical_mv = ?, overcharge_on = ?, overcharge_mv = ?
			WHERE watch_id = ?`,
			nullMV(lowMV), nullMV(criticalMV), boolInt(overOn), nullMV(overMV), watchID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		if !overOn {
			_, err = tx.ExecContext(ctx, `
				UPDATE battery_rules SET level = 'unknown', since = ?, candidate = '', candidate_count = 0,
					announced = '', last_reading_at = NULL
				WHERE watch_id = ? AND level = 'overcharged'`, now.Unix(), watchID)
		}
		return err
	})
}

// SetAutoBattery chooses whether battery rules are added to a person's
// repeaters by themselves. It never removes rules they already have.
func (s *Store) SetAutoBattery(ctx context.Context, userID int64, on bool) error {
	_, err := s.write.ExecContext(ctx, `UPDATE users SET auto_battery = ? WHERE id = ?`, boolInt(on), userID)
	return err
}

// DismissBatteryBanner hides the one-off banner.
func (s *Store) DismissBatteryBanner(ctx context.Context, userID int64) error {
	_, err := s.write.ExecContext(ctx, `UPDATE users SET battery_banner = 0 WHERE id = ?`, userID)
	return err
}

func ownedTx(ctx context.Context, tx *sql.Tx, userID, watchID int64) error {
	var one int
	err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM watches WHERE id = ? AND user_id = ?`, watchID, userID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// SaveBatteryReadings stores the latest report for one repeater.
func (s *Store) SaveBatteryReadings(ctx context.Context, r BatteryReadings) error {
	raw, err := json.Marshal(corescope.PackSamples(r.Samples))
	if err != nil {
		return err
	}
	_, err = s.write.ExecContext(ctx, `
		INSERT INTO battery_readings (key, status, latest_mv, latest_at, reporter, stale,
			cs_low_mv, cs_critical_mv, samples, fetched_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(key) DO UPDATE SET status = excluded.status, latest_mv = excluded.latest_mv,
			latest_at = excluded.latest_at, reporter = excluded.reporter, stale = excluded.stale,
			cs_low_mv = excluded.cs_low_mv, cs_critical_mv = excluded.cs_critical_mv,
			samples = excluded.samples, fetched_at = excluded.fetched_at`,
		r.Key, string(r.Status), r.LatestMV, nullInt(r.LatestAt), r.Reporter, boolInt(r.Stale),
		r.CSLowMV, r.CSCriticalMV, string(raw), r.FetchedAt.Unix())
	return err
}

// BatteryReadingsFor returns the stored reports for the given repeaters,
// keyed by repeater key.
func (s *Store) BatteryReadingsFor(ctx context.Context, keys []string) (map[string]BatteryReadings, error) {
	out := map[string]BatteryReadings{}
	if len(keys) == 0 {
		return out, nil
	}
	ph := make([]string, len(keys))
	args := make([]any, len(keys))
	for i, k := range keys {
		ph[i], args[i] = "?", k
	}
	rows, err := s.read.QueryContext(ctx, `
		SELECT key, status, latest_mv, latest_at, reporter, stale, cs_low_mv, cs_critical_mv,
			samples, fetched_at FROM battery_readings WHERE key IN (`+strings.Join(ph, ",")+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r BatteryReadings
		var status, samples string
		var latest sql.NullInt64
		var stale int
		var fetched int64
		if err := rows.Scan(&r.Key, &status, &r.LatestMV, &latest, &r.Reporter, &stale,
			&r.CSLowMV, &r.CSCriticalMV, &samples, &fetched); err != nil {
			return nil, err
		}
		r.Status = corescope.BatteryStatus(status)
		r.LatestAt = timeFrom(latest)
		r.Stale = stale != 0
		r.FetchedAt = time.Unix(fetched, 0).UTC()
		var pts [][2]int64
		if err := json.Unmarshal([]byte(samples), &pts); err != nil {
			return nil, err
		}
		r.Samples = corescope.UnpackSamples(pts)
		out[r.Key] = r
	}
	return out, rows.Err()
}

// PruneBatteryReadings drops readings for repeaters nobody watches any more.
func (s *Store) PruneBatteryReadings(ctx context.Context) error {
	_, err := s.write.ExecContext(ctx, `
		DELETE FROM battery_readings
		WHERE key NOT IN (SELECT target_key FROM watches WHERE target_kind = 'node')`)
	return err
}

// nullMV stores 0 as NULL, meaning "not set".
func nullMV(mv int) any {
	if mv == 0 {
		return nil
	}
	return mv
}
