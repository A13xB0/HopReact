package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"hopreact/internal/corescope"
)

// ErrNotFound is returned when a lookup finds nothing.
var ErrNotFound = errors.New("store: not found")

// ---------------------------------------------------------------- users --

// UpsertUser records a Discord sign-in, creating the account on first use.
// The display name and avatar are refreshed every time, since Discord is the
// authority on both and people change them.
func (s *Store) UpsertUser(ctx context.Context, discordID, username, avatar string) (User, error) {
	now := s.Now().UTC()
	err := s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO users (discord_id, username, avatar, created_at, last_login_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(discord_id) DO UPDATE SET
				username = excluded.username,
				avatar = excluded.avatar,
				last_login_at = excluded.last_login_at`,
			discordID, username, nullString(avatar), now.Unix(), now.Unix())
		return err
	})
	if err != nil {
		return User{}, fmt.Errorf("store: upserting user: %w", err)
	}
	return s.UserByDiscordID(ctx, discordID)
}

const userCols = `id, discord_id, username, COALESCE(avatar,''), created_at,
	last_login_at, dm_ok, COALESCE(dm_failed_reason,''), dm_checked_at`

func scanUser(row interface{ Scan(...any) error }) (User, error) {
	var u User
	var created, login int64
	var dmOK int
	var checked sql.NullInt64
	if err := row.Scan(&u.ID, &u.DiscordID, &u.Username, &u.Avatar, &created,
		&login, &dmOK, &u.DMFailedReason, &checked); err != nil {
		return User{}, err
	}
	u.CreatedAt = time.Unix(created, 0).UTC()
	u.LastLoginAt = time.Unix(login, 0).UTC()
	u.DMOK = dmOK != 0
	u.DMCheckedAt = timeFrom(checked)
	return u, nil
}

func (s *Store) UserByDiscordID(ctx context.Context, discordID string) (User, error) {
	row := s.read.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE discord_id = ?`, discordID)
	u, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}

func (s *Store) UserByID(ctx context.Context, id int64) (User, error) {
	row := s.read.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id = ?`, id)
	u, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}

// SetDMStatus records whether we can currently reach this user. Called after
// a send attempt and after an explicit membership check — a user whose
// alerts are silently undeliverable is worse off than one with none, so this
// drives a banner rather than just a log line.
func (s *Store) SetDMStatus(ctx context.Context, userID int64, ok bool, reason string) error {
	v := 0
	if ok {
		v = 1
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE users SET dm_ok = ?, dm_failed_reason = ?, dm_checked_at = ? WHERE id = ?`,
			v, nullString(reason), s.Now().UTC().Unix(), userID)
		return err
	})
}

// DeleteUser removes an account and, by cascade, its sessions, watches,
// watch state and queued notifications.
func (s *Store) DeleteUser(ctx context.Context, userID int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, userID)
		return err
	})
}

// ------------------------------------------------------------- sessions --

// CreateSession stores a session by the SHA-256 of its token, so the raw
// cookie value never touches the database.
func (s *Store) CreateSession(ctx context.Context, tokenHash []byte, userID int64, csrf string, expires time.Time) error {
	now := s.Now().UTC()
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO sessions (token_hash, user_id, csrf_token, created_at, expires_at)
			 VALUES (?, ?, ?, ?, ?)`,
			tokenHash, userID, csrf, now.Unix(), expires.Unix())
		return err
	})
}

// SessionUser resolves a session token hash to its user, treating an expired
// session as absent.
func (s *Store) SessionUser(ctx context.Context, tokenHash []byte) (User, string, error) {
	var csrf string
	var expires int64
	var userID int64
	err := s.read.QueryRowContext(ctx,
		`SELECT user_id, csrf_token, expires_at FROM sessions WHERE token_hash = ?`,
		tokenHash).Scan(&userID, &csrf, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, "", ErrNotFound
	}
	if err != nil {
		return User{}, "", err
	}
	if s.Now().UTC().Unix() >= expires {
		return User{}, "", ErrNotFound
	}
	u, err := s.UserByID(ctx, userID)
	return u, csrf, err
}

func (s *Store) DeleteSession(ctx context.Context, tokenHash []byte) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, tokenHash)
		return err
	})
}

// DeleteExpiredSessions is periodic housekeeping.
func (s *Store) DeleteExpiredSessions(ctx context.Context) (int64, error) {
	var n int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at < ?`, s.Now().UTC().Unix())
		if err != nil {
			return err
		}
		n, _ = res.RowsAffected()
		return nil
	})
	return n, err
}

// -------------------------------------------------------------- targets --

// UpsertTargets writes one poll's observations.
//
// Timestamps only ever move forward. CoreScope can serve a stale or partial
// view, and letting a target's last_seen regress would manufacture a false
// alert out of nothing. AdvancedCount reports how many genuinely moved,
// which is what the frozen-feed check reads: a full, well-formed response in
// which nothing at all advanced is far more likely to be a broken upstream
// than an entire mesh going silent between two polls.
func (s *Store) UpsertTargets(ctx context.Context, obs []corescope.Observation) (advanced int, err error) {
	now := s.Now().UTC().Unix()
	err = s.tx(ctx, func(tx *sql.Tx) error {
		sel, err := tx.PrepareContext(ctx,
			`SELECT last_seen_at, last_relayed_at, relay_ever_observed_at FROM targets WHERE kind = ? AND key = ?`)
		if err != nil {
			return err
		}
		defer sel.Close()

		ins, err := tx.PrepareContext(ctx, `
			INSERT INTO targets (kind, key, name, role, last_seen_at, last_relayed_at,
				relay_ever_observed_at, relay_count_1h, relay_count_24h, lat, lon,
				first_indexed_at, last_in_feed_at, updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(kind, key) DO UPDATE SET
				name = excluded.name,
				role = excluded.role,
				last_seen_at = excluded.last_seen_at,
				last_relayed_at = excluded.last_relayed_at,
				relay_ever_observed_at = excluded.relay_ever_observed_at,
				relay_count_1h = excluded.relay_count_1h,
				relay_count_24h = excluded.relay_count_24h,
				lat = excluded.lat, lon = excluded.lon,
				last_in_feed_at = excluded.last_in_feed_at,
				updated_at = excluded.updated_at`)
		if err != nil {
			return err
		}
		defer ins.Close()

		for _, o := range obs {
			var prevSeen, prevRelayed, prevEver sql.NullInt64
			switch err := sel.QueryRowContext(ctx, string(o.Kind), o.Key).
				Scan(&prevSeen, &prevRelayed, &prevEver); {
			case errors.Is(err, sql.ErrNoRows):
				// first sighting
			case err != nil:
				return err
			}

			seen := maxTime(timeFrom(prevSeen), o.LastSeen)
			relayed := maxTime(timeFrom(prevRelayed), o.LastRelayed)
			if !timeFrom(prevSeen).IsZero() && seen.After(timeFrom(prevSeen)) {
				advanced++
			}

			// Once a target has relayed, it has relayed — the flag is
			// sticky so a later feed that omits last_relayed can't make an
			// established relay look like one that never started.
			ever := timeFrom(prevEver)
			if ever.IsZero() && !relayed.IsZero() {
				ever = relayed
			}

			if _, err := ins.ExecContext(ctx,
				string(o.Kind), o.Key, o.Name, o.Role,
				nullInt(seen), nullInt(relayed), nullInt(ever),
				o.RelayCount1h, o.RelayCount24h, o.Lat, o.Lon,
				now, now, now); err != nil {
				return err
			}
		}
		return nil
	})
	return advanced, err
}

const targetCols = `kind, key, name, role, last_seen_at, last_relayed_at,
	relay_ever_observed_at, relay_count_1h, relay_count_24h, lat, lon,
	first_indexed_at, last_in_feed_at, updated_at`

func scanTarget(row interface{ Scan(...any) error }) (Target, error) {
	var t Target
	var seen, relayed, ever sql.NullInt64
	var first, feed, updated int64
	if err := row.Scan(&t.Kind, &t.Key, &t.Name, &t.Role, &seen, &relayed, &ever,
		&t.RelayCount1h, &t.RelayCount24h, &t.Lat, &t.Lon, &first, &feed, &updated); err != nil {
		return Target{}, err
	}
	t.LastSeen = timeFrom(seen)
	t.LastRelayed = timeFrom(relayed)
	t.RelayEverObserved = timeFrom(ever)
	t.FirstIndexedAt = time.Unix(first, 0).UTC()
	t.LastInFeedAt = time.Unix(feed, 0).UTC()
	t.UpdatedAt = time.Unix(updated, 0).UTC()
	return t, nil
}

func (s *Store) Target(ctx context.Context, kind, key string) (Target, error) {
	row := s.read.QueryRowContext(ctx,
		`SELECT `+targetCols+` FROM targets WHERE kind = ? AND key = ?`, kind, strings.ToLower(key))
	t, err := scanTarget(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Target{}, ErrNotFound
	}
	return t, err
}

// SearchTargets backs the picker. An empty query lists the most recently
// seen, which is the useful default — a user is far more likely to be
// looking for a live node than a long-dead one.
func (s *Store) SearchTargets(ctx context.Context, q string, limit int) ([]Target, error) {
	if limit <= 0 {
		limit = 25
	}
	var (
		rows *sql.Rows
		err  error
	)
	if strings.TrimSpace(q) == "" {
		rows, err = s.read.QueryContext(ctx,
			`SELECT `+targetCols+` FROM targets ORDER BY last_seen_at DESC NULLS LAST LIMIT ?`, limit)
	} else {
		like := "%" + strings.ToLower(strings.TrimSpace(q)) + "%"
		rows, err = s.read.QueryContext(ctx,
			`SELECT `+targetCols+` FROM targets
			 WHERE lower(name) LIKE ? OR key LIKE ?
			 ORDER BY last_seen_at DESC NULLS LAST LIMIT ?`, like, like, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Target
	for rows.Next() {
		t, err := scanTarget(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) CountTargets(ctx context.Context) (int, error) {
	var n int
	err := s.read.QueryRowContext(ctx, `SELECT COUNT(*) FROM targets`).Scan(&n)
	return n, err
}

// -------------------------------------------------------------- watches --

// ErrWatchLimit is returned when an account is already at its cap.
var ErrWatchLimit = errors.New("store: watch limit reached")

// ErrDuplicateWatch is returned when this user already watches this target.
var ErrDuplicateWatch = errors.New("store: already watching this target")

// CreateWatch adds a subscription and seeds its state.
//
// Seeding is the interesting part. If the target is ALREADY past the
// threshold, the watch starts in 'alerting' with notify_count = 0 and sends
// nothing: nobody wants an alert for something that was broken before they
// asked to watch it. Because notify_count is zero the eventual recovery is
// silent too — you never announce recovery from an alert that was never
// announced. On this network that is the common path, not an edge case: 526
// of 779 targets have not been seen in over 24 hours.
func (s *Store) CreateWatch(ctx context.Context, w Watch, maxPerUser int) (int64, error) {
	now := s.Now().UTC()
	var id int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM watches WHERE user_id = ?`, w.UserID).Scan(&n); err != nil {
			return err
		}
		if n >= maxPerUser {
			return ErrWatchLimit
		}

		res, err := tx.ExecContext(ctx, `
			INSERT INTO watches (user_id, target_kind, target_key, threshold_hours,
				alert_on_relay, label, muted_until, created_at)
			VALUES (?,?,?,?,?,?,?,?)`,
			w.UserID, w.TargetKind, strings.ToLower(w.TargetKey), w.ThresholdHours,
			boolInt(w.AlertOnRelay), nullString(w.Label), nullInt(w.MutedUntil), now.Unix())
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return ErrDuplicateWatch
			}
			return err
		}
		id, err = res.LastInsertId()
		if err != nil {
			return err
		}

		// Seed from whatever we already know about the target.
		var seen, relayed, ever sql.NullInt64
		err = tx.QueryRowContext(ctx,
			`SELECT last_seen_at, last_relayed_at, relay_ever_observed_at FROM targets WHERE kind = ? AND key = ?`,
			w.TargetKind, strings.ToLower(w.TargetKey)).Scan(&seen, &relayed, &ever)
		known := !errors.Is(err, sql.ErrNoRows)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		threshold := time.Duration(w.ThresholdHours) * time.Hour
		seedSignal := func(sig Signal, ts time.Time, applicable bool) error {
			st := StateUnknown
			seeded := 0
			if applicable && known && !ts.IsZero() {
				if now.Sub(ts) >= threshold {
					st = StateAlerting
					seeded = 1
				} else {
					st = StateOK
				}
			}
			var alertingSince any
			if st == StateAlerting {
				alertingSince = now.Unix()
			}
			_, err := tx.ExecContext(ctx, `
				INSERT INTO watch_state (watch_id, signal, state, since, consecutive,
					observed_at, alerting_since, notify_count, seeded)
				VALUES (?,?,?,?,0,?,?,0,?)`,
				id, string(sig), string(st), now.Unix(), nullInt(ts), alertingSince, seeded)
			return err
		}

		if err := seedSignal(SignalSeen, timeFrom(seen), true); err != nil {
			return err
		}
		// The relay signal is only meaningful once the target has actually
		// been observed relaying.
		relayApplicable := w.AlertOnRelay && !timeFrom(ever).IsZero()
		return seedSignal(SignalRelayed, timeFrom(relayed), relayApplicable)
	})
	return id, err
}

func (s *Store) DeleteWatch(ctx context.Context, userID, watchID int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM watches WHERE id = ? AND user_id = ?`, watchID, userID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// UpdateWatch changes the tunable parts of a subscription.
func (s *Store) UpdateWatch(ctx context.Context, userID, watchID int64, thresholdHours int, alertOnRelay bool, mutedUntil time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE watches SET threshold_hours = ?, alert_on_relay = ?, muted_until = ?
			 WHERE id = ? AND user_id = ?`,
			thresholdHours, boolInt(alertOnRelay), nullInt(mutedUntil), watchID, userID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		// Turning the relay signal on for a target that has relayed before
		// needs a row to exist; turning it off leaves the row dormant.
		if alertOnRelay {
			_, err = tx.ExecContext(ctx, `
				INSERT INTO watch_state (watch_id, signal, state, since, consecutive, notify_count, seeded)
				SELECT ?, 'relayed', 'unknown', ?, 0, 0, 0
				WHERE NOT EXISTS (SELECT 1 FROM watch_state WHERE watch_id = ? AND signal = 'relayed')`,
				watchID, s.Now().UTC().Unix(), watchID)
		}
		return err
	})
}

const watchCols = `id, user_id, target_kind, target_key, threshold_hours,
	alert_on_relay, COALESCE(label,''), muted_until, created_at`

func scanWatch(row interface{ Scan(...any) error }) (Watch, error) {
	var w Watch
	var relay int
	var muted sql.NullInt64
	var created int64
	if err := row.Scan(&w.ID, &w.UserID, &w.TargetKind, &w.TargetKey, &w.ThresholdHours,
		&relay, &w.Label, &muted, &created); err != nil {
		return Watch{}, err
	}
	w.AlertOnRelay = relay != 0
	w.MutedUntil = timeFrom(muted)
	w.CreatedAt = time.Unix(created, 0).UTC()
	return w, nil
}

// ActiveWatches returns every watch, for the evaluator.
func (s *Store) ActiveWatches(ctx context.Context) ([]Watch, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT `+watchCols+` FROM watches`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Watch
	for rows.Next() {
		w, err := scanWatch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (s *Store) CountWatches(ctx context.Context) (int, error) {
	var n int
	err := s.read.QueryRowContext(ctx, `SELECT COUNT(*) FROM watches`).Scan(&n)
	return n, err
}

// ---------------------------------------------------------- watch state --

const stateCols = `watch_id, signal, state, since, consecutive, observed_at,
	alerting_since, last_notified_at, notify_count, seeded`

func scanState(row interface{ Scan(...any) error }) (WatchState, error) {
	var st WatchState
	var sig, state string
	var since int64
	var observed, alerting, notified sql.NullInt64
	var seeded int
	if err := row.Scan(&st.WatchID, &sig, &state, &since, &st.Consecutive,
		&observed, &alerting, &notified, &st.NotifyCount, &seeded); err != nil {
		return WatchState{}, err
	}
	st.Signal = Signal(sig)
	st.State = State(state)
	st.Since = time.Unix(since, 0).UTC()
	st.ObservedAt = timeFrom(observed)
	st.AlertingSince = timeFrom(alerting)
	st.LastNotified = timeFrom(notified)
	st.Seeded = seeded != 0
	return st, nil
}

// AllWatchState returns every state row keyed by watch id and signal.
func (s *Store) AllWatchState(ctx context.Context) (map[int64]map[Signal]WatchState, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT `+stateCols+` FROM watch_state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]map[Signal]WatchState{}
	for rows.Next() {
		st, err := scanState(rows)
		if err != nil {
			return nil, err
		}
		if out[st.WatchID] == nil {
			out[st.WatchID] = map[Signal]WatchState{}
		}
		out[st.WatchID][st.Signal] = st
	}
	return out, rows.Err()
}

// SaveWatchState persists one evaluated state row.
func (s *Store) SaveWatchState(ctx context.Context, tx *sql.Tx, st WatchState) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO watch_state (watch_id, signal, state, since, consecutive,
			observed_at, alerting_since, last_notified_at, notify_count, seeded)
		VALUES (?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(watch_id, signal) DO UPDATE SET
			state = excluded.state, since = excluded.since,
			consecutive = excluded.consecutive, observed_at = excluded.observed_at,
			alerting_since = excluded.alerting_since,
			last_notified_at = excluded.last_notified_at,
			notify_count = excluded.notify_count, seeded = excluded.seeded`,
		st.WatchID, string(st.Signal), string(st.State), st.Since.UTC().Unix(),
		st.Consecutive, nullInt(st.ObservedAt), nullInt(st.AlertingSince),
		nullInt(st.LastNotified), st.NotifyCount, boolInt(st.Seeded))
	return err
}

// ------------------------------------------------- poll runs & outbox ----

func (s *Store) StartPollRun(ctx context.Context) (int64, error) {
	var id int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO poll_runs (started_at, status) VALUES (?, 'failed')`,
			s.Now().UTC().Unix())
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	return id, err
}

func (s *Store) FinishPollRun(ctx context.Context, run PollRun) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE poll_runs SET finished_at = ?, status = ?, node_count = ?,
				observer_count = ?, advanced_count = ?, evaluated = ?,
				suppressed_reason = ?, error = ? WHERE id = ?`,
			s.Now().UTC().Unix(), string(run.Status), run.NodeCount, run.ObserverCount,
			run.AdvancedCount, boolInt(run.Evaluated), nullString(run.SuppressedReason),
			nullString(run.Error), run.ID)
		return err
	})
}

// RecentPollRuns backs the upstream-health banner.
func (s *Store) RecentPollRuns(ctx context.Context, limit int) ([]PollRun, error) {
	rows, err := s.read.QueryContext(ctx, `
		SELECT id, started_at, finished_at, status, node_count, observer_count,
			advanced_count, evaluated, COALESCE(suppressed_reason,''), COALESCE(error,'')
		FROM poll_runs ORDER BY started_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PollRun
	for rows.Next() {
		var r PollRun
		var started int64
		var finished sql.NullInt64
		var status string
		var evaluated int
		if err := rows.Scan(&r.ID, &started, &finished, &status, &r.NodeCount,
			&r.ObserverCount, &r.AdvancedCount, &evaluated, &r.SuppressedReason, &r.Error); err != nil {
			return nil, err
		}
		r.StartedAt = time.Unix(started, 0).UTC()
		r.FinishedAt = timeFrom(finished)
		r.Status = PollStatus(status)
		r.Evaluated = evaluated != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// MaxRecentNodeCount is the yardstick the "implausibly small poll" check
// measures against.
func (s *Store) MaxRecentNodeCount(ctx context.Context, since time.Time) (int, error) {
	var n sql.NullInt64
	err := s.read.QueryRowContext(ctx,
		`SELECT MAX(node_count) FROM poll_runs WHERE status = 'ok' AND started_at >= ?`,
		since.UTC().Unix()).Scan(&n)
	if err != nil {
		return 0, err
	}
	if !n.Valid {
		return 0, nil
	}
	return int(n.Int64), nil
}

// ConsecutiveNonAdvancingPolls counts back from the most recent poll to see
// how long the feed has looked frozen.
func (s *Store) ConsecutiveNonAdvancingPolls(ctx context.Context) (int, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT advanced_count FROM poll_runs WHERE status IN ('ok','suspect') ORDER BY started_at DESC LIMIT 20`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var advanced int
		if err := rows.Scan(&advanced); err != nil {
			return 0, err
		}
		if advanced > 0 {
			break
		}
		n++
	}
	return n, rows.Err()
}

// QueueNotification appends to the outbox inside the caller's transaction,
// so a message is only ever queued if the state change that justified it
// also committed.
func (s *Store) QueueNotification(ctx context.Context, tx *sql.Tx, userID int64, kind, payload string, sendAfter time.Time) error {
	now := s.Now().UTC()
	if sendAfter.IsZero() {
		sendAfter = now
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO notifications (user_id, kind, payload, created_at, send_after)
		 VALUES (?,?,?,?,?)`,
		userID, kind, payload, now.Unix(), sendAfter.UTC().Unix())
	return err
}

// PendingNotifications returns due, unsent messages joined to the Discord id
// they should go to.
func (s *Store) PendingNotifications(ctx context.Context, limit int) ([]Notification, error) {
	rows, err := s.read.QueryContext(ctx, `
		SELECT n.id, n.user_id, u.discord_id, n.kind, n.payload, n.created_at,
			n.send_after, n.attempts, COALESCE(n.last_error,'')
		FROM notifications n JOIN users u ON u.id = n.user_id
		WHERE n.sent_at IS NULL AND n.send_after <= ?
		ORDER BY n.id LIMIT ?`, s.Now().UTC().Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Notification
	for rows.Next() {
		var n Notification
		var created, after int64
		if err := rows.Scan(&n.ID, &n.UserID, &n.DiscordID, &n.Kind, &n.Payload,
			&created, &after, &n.Attempts, &n.LastError); err != nil {
			return nil, err
		}
		n.CreatedAt = time.Unix(created, 0).UTC()
		n.SendAfter = time.Unix(after, 0).UTC()
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *Store) MarkNotificationSent(ctx context.Context, id int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE notifications SET sent_at = ? WHERE id = ?`,
			s.Now().UTC().Unix(), id)
		return err
	})
}

// MarkNotificationFailed records an attempt and backs off exponentially, so
// a transient Discord outage doesn't spin.
func (s *Store) MarkNotificationFailed(ctx context.Context, id int64, attempts int, reason string) error {
	backoff := time.Duration(1<<minInt(attempts, 6)) * time.Minute
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE notifications SET attempts = attempts + 1, last_error = ?, send_after = ? WHERE id = ?`,
			reason, s.Now().UTC().Add(backoff).Unix(), id)
		return err
	})
}

// DropNotification abandons a message that can never be delivered — an
// undeliverable DM is recorded on the user instead, where the dashboard can
// show it.
func (s *Store) DropNotification(ctx context.Context, id int64, reason string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE notifications SET sent_at = ?, last_error = ? WHERE id = ?`,
			s.Now().UTC().Unix(), reason, id)
		return err
	})
}

func (s *Store) RecordAlertEvent(ctx context.Context, tx *sql.Tx, watchID int64, sig Signal, from, to State, pollRunID int64, notified bool) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO alert_events (watch_id, signal, from_state, to_state, at, poll_run_id, notified)
		 VALUES (?,?,?,?,?,?,?)`,
		watchID, string(sig), string(from), string(to), s.Now().UTC().Unix(), pollRunID, boolInt(notified))
	return err
}

// WriteTx exposes a write transaction so the poller can commit an entire
// evaluation — state rows, events and queued notifications — atomically.
func (s *Store) WriteTx(ctx context.Context, fn func(*sql.Tx) error) error {
	return s.tx(ctx, fn)
}

// ---------------------------------------------------------- dashboard ----

// WatchViews returns everything the dashboard needs for one user in a single
// query per table rather than N+1 lookups.
func (s *Store) WatchViews(ctx context.Context, userID int64) ([]WatchView, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT `+watchCols+` FROM watches WHERE user_id = ? ORDER BY id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var views []WatchView
	for rows.Next() {
		w, err := scanWatch(rows)
		if err != nil {
			return nil, err
		}
		views = append(views, WatchView{Watch: w})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(views) == 0 {
		return nil, nil
	}

	states, err := s.AllWatchState(ctx)
	if err != nil {
		return nil, err
	}
	for i := range views {
		byS := states[views[i].Watch.ID]
		views[i].Seen = byS[SignalSeen]
		if views[i].Watch.AlertOnRelay {
			if st, ok := byS[SignalRelayed]; ok {
				stCopy := st
				views[i].Relay = &stCopy
			}
		}
		t, err := s.Target(ctx, views[i].Watch.TargetKind, views[i].Watch.TargetKey)
		if err == nil {
			tCopy := t
			views[i].Target = &tCopy
		} else if !errors.Is(err, ErrNotFound) {
			return nil, err
		}
	}
	return views, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// UsersWithWatches returns the accounts that actually have something being
// watched — the only ones whose Discord reachability is worth re-checking.
func (s *Store) UsersWithWatches(ctx context.Context) ([]User, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT `+userCols+` FROM users u
		 WHERE EXISTS (SELECT 1 FROM watches w WHERE w.user_id = u.id)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
