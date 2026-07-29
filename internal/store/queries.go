package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
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
			`SELECT last_seen_at, last_relayed_at, relay_ever_observed_at, last_packet_at FROM targets WHERE kind = ? AND key = ?`)
		if err != nil {
			return err
		}
		defer sel.Close()

		ins, err := tx.PrepareContext(ctx, `
			INSERT INTO targets (kind, key, name, role, last_seen_at, last_relayed_at,
				relay_ever_observed_at, last_packet_at, relay_count_1h, relay_count_24h, lat, lon,
				bridge_score, traffic_share,
				first_indexed_at, last_in_feed_at, updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(kind, key) DO UPDATE SET
				name = excluded.name,
				role = excluded.role,
				last_seen_at = excluded.last_seen_at,
				last_relayed_at = excluded.last_relayed_at,
				relay_ever_observed_at = excluded.relay_ever_observed_at,
				last_packet_at = excluded.last_packet_at,
				bridge_score = excluded.bridge_score,
				traffic_share = excluded.traffic_share,
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
			var prevSeen, prevRelayed, prevEver, prevPacket sql.NullInt64
			switch err := sel.QueryRowContext(ctx, string(o.Kind), o.Key).
				Scan(&prevSeen, &prevRelayed, &prevEver, &prevPacket); {
			case errors.Is(err, sql.ErrNoRows):
				// first sighting
			case err != nil:
				return err
			}

			seen := maxTime(timeFrom(prevSeen), o.LastSeen)
			relayed := maxTime(timeFrom(prevRelayed), o.LastRelayed)
			packet := maxTime(timeFrom(prevPacket), o.LastPacket)
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
				nullInt(seen), nullInt(relayed), nullInt(ever), nullInt(packet),
				o.RelayCount1h, o.RelayCount24h, o.Lat, o.Lon,
				o.BridgeScore, o.TrafficShare,
				now, now, now); err != nil {
				return err
			}
		}
		return nil
	})
	return advanced, err
}

const targetCols = `kind, key, name, role, last_seen_at, last_relayed_at,
	relay_ever_observed_at, last_packet_at, relay_count_1h, relay_count_24h, lat, lon,
	first_indexed_at, last_in_feed_at, updated_at`

func scanTarget(row interface{ Scan(...any) error }) (Target, error) {
	var t Target
	var seen, relayed, ever, packet sql.NullInt64
	var first, feed, updated int64
	if err := row.Scan(&t.Kind, &t.Key, &t.Name, &t.Role, &seen, &relayed, &ever, &packet,
		&t.RelayCount1h, &t.RelayCount24h, &t.Lat, &t.Lon, &first, &feed, &updated); err != nil {
		return Target{}, err
	}
	t.LastSeen = timeFrom(seen)
	t.LastRelayed = timeFrom(relayed)
	t.RelayEverObserved = timeFrom(ever)
	t.LastPacket = timeFrom(packet)
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

// DefaultRules returns the rules a newly created watch starts with.
//
// Observers get CoreScope's plain "heard at all". They never appear in packet
// routes — an observer reports what it hears rather than forwarding it — so
// per-type attribution has nothing to say about one.
//
// Nodes get the per-type default: adverts, responses and channel messages,
// counting both traffic the node originated and traffic it carried. Adverts
// anchor that set deliberately. A node states its own public key outright in
// an advert, so that part of the signal needs no path hashes at all and
// cannot be starved by a run of narrow hop widths.
func DefaultRules(kind string, thresholdHours int, alertOnRelay bool) []Rule {
	if kind == string(corescope.KindObserver) {
		// An observer's own check-in, not what it has heard. Whether it hears
		// anything depends on how busy the mesh around it is; whether it
		// checks in depends on whether it is working.
		return []Rule{{
			Label: "Stopped checking in", Source: SourceSeen,
			Direction: DirEither, ThresholdHours: thresholdHours,
		}}
	}
	rules := []Rule{
		{
			Label:  "Adverts, responses or channel messages",
			Source: SourceTypes,
			Types: []int{
				corescope.TypeADVERT, corescope.TypeRESPONSE, corescope.TypeGRPTXT,
			},
			Direction:      DirEither,
			ThresholdHours: thresholdHours,
		},
		// The backstop, and it is not optional. The rule above can only see
		// adverts and path hops at least three bytes wide, so a node whose
		// traffic never happens to be attributable produces no evidence — and
		// a rule with no evidence deliberately stays quiet rather than
		// guessing. That is the right call for precision and the wrong one
		// for safety, because it would leave someone believing they were
		// covered when nothing could ever fire.
		//
		// This rule reads CoreScope's own last_seen, which cannot be starved
		// that way. It sits at the same threshold, and because it sees strictly
		// more traffic it always triggers later than the rule above would have
		// — so in normal operation the precise rule speaks first and this one
		// never gets a word in. It only matters when the precise rule is blind.
		{
			Label:          "Not heard at all",
			Source:         SourceSeen,
			Direction:      DirEither,
			ThresholdHours: thresholdHours,
		},
	}
	if alertOnRelay {
		// The old opt-in, still offered when adding a node. Uses CoreScope's
		// own relay figure rather than per-type evidence, so it stays exactly
		// the signal it has always been.
		rules = append(rules, Rule{
			Label:          "Stopped passing traffic",
			Source:         SourceRelayed,
			Direction:      DirCarried,
			ThresholdHours: thresholdHours,
		})
	}
	return rules
}

// CreateWatch adds a subscription, gives it the default rules and seeds their
// state.
//
// Seeding is the interesting part. If a rule is ALREADY past its threshold,
// it starts in 'alerting' with notify_count = 0 and sends nothing: nobody
// wants an alert for something that was broken before they asked to watch it.
// Because notify_count is zero the eventual recovery is silent too — you never
// announce recovery from an alert that was never announced. On this network
// that is the common path, not an edge case: 526 of 779 targets have not been
// seen in over 24 hours.
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
				alert_on_relay, label, muted_until, notify_recovery, created_at)
			VALUES (?,?,?,?,?,?,?,?,?)`,
			w.UserID, w.TargetKind, strings.ToLower(w.TargetKey), w.ThresholdHours,
			boolInt(w.AlertOnRelay), nullString(w.Label), nullInt(w.MutedUntil),
			boolInt(w.NotifyRecovery), now.Unix())
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

		for _, r := range DefaultRules(w.TargetKind, w.ThresholdHours, w.AlertOnRelay) {
			r.WatchID = id
			if _, err := s.insertRuleTx(ctx, tx, r, w.TargetKind, strings.ToLower(w.TargetKey), now); err != nil {
				return err
			}
		}
		return nil
	})
	return id, err
}

// insertRuleTx writes a rule and seeds its alert state from whatever is
// already known about the target.
func (s *Store) insertRuleTx(ctx context.Context, tx *sql.Tx, r Rule, kind, key string, now time.Time) (int64, error) {
	res, err := tx.ExecContext(ctx, `
		INSERT INTO watch_rules (watch_id, label, source, types, direction,
			threshold_hours, created_at)
		VALUES (?,?,?,?,?,?,?)`,
		r.WatchID, r.Label, string(r.Source), encodeTypes(r.Types),
		string(r.Direction), r.ThresholdHours, now.Unix())
	if err != nil {
		return 0, err
	}
	ruleID, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}

	at, applicable, err := ruleSeedObservation(ctx, tx, kind, key, r)
	if err != nil {
		return 0, err
	}

	st := StateUnknown
	seeded := 0
	if applicable && !at.IsZero() {
		if now.Sub(at) >= time.Duration(r.ThresholdHours)*time.Hour {
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
	_, err = tx.ExecContext(ctx, `
		INSERT INTO watch_state (watch_id, rule_id, state, since, consecutive,
			observed_at, alerting_since, notify_count, seeded)
		VALUES (?,?,?,?,0,?,?,0,?)`,
		r.WatchID, ruleID, string(st), now.Unix(), nullInt(at), alertingSince, seeded)
	return ruleID, err
}

// ruleSeedObservation reads the timestamp a rule would evaluate right now.
//
// applicable is false when the rule has nothing to judge: a target CoreScope
// has never mentioned, a node that has never been observed relaying, or — for
// a per-type rule — a target for which none of the selected types has ever
// produced evidence. All three must stay unknown rather than being read as
// silence, because absence of evidence is not absence of the node.
func ruleSeedObservation(ctx context.Context, tx *sql.Tx, kind, key string, r Rule) (time.Time, bool, error) {
	switch r.Source {
	case SourceSeen:
		var seen sql.NullInt64
		err := tx.QueryRowContext(ctx,
			`SELECT last_seen_at FROM targets WHERE kind = ? AND key = ?`, kind, key).Scan(&seen)
		if errors.Is(err, sql.ErrNoRows) {
			return time.Time{}, false, nil
		}
		return timeFrom(seen), err == nil, err

	case SourceRelayed:
		var relayed, ever sql.NullInt64
		err := tx.QueryRowContext(ctx,
			`SELECT last_relayed_at, relay_ever_observed_at FROM targets WHERE kind = ? AND key = ?`,
			kind, key).Scan(&relayed, &ever)
		if errors.Is(err, sql.ErrNoRows) {
			return time.Time{}, false, nil
		}
		if err != nil {
			return time.Time{}, false, err
		}
		// A node that has never relayed has not stopped relaying.
		return timeFrom(relayed), !timeFrom(ever).IsZero(), nil

	case SourcePackets:
		var at sql.NullInt64
		err := tx.QueryRowContext(ctx,
			`SELECT last_packet_at FROM targets WHERE kind = ? AND key = ?`, kind, key).Scan(&at)
		if errors.Is(err, sql.ErrNoRows) {
			return time.Time{}, false, nil
		}
		return timeFrom(at), err == nil && at.Valid, err

	case SourceTypes:
		q, args := activityQuery(kind, key, r)
		if q == "" {
			return time.Time{}, false, nil
		}
		var at sql.NullInt64
		if err := tx.QueryRowContext(ctx, q, args...).Scan(&at); err != nil {
			return time.Time{}, false, err
		}
		return timeFrom(at), at.Valid, nil
	}
	return time.Time{}, false, nil
}

// activityQuery builds the MAX(last_at) lookup for a per-type rule. Returns
// an empty query when the rule selects nothing, which can only happen if a
// rule was stored with no types.
func activityQuery(kind, key string, r Rule) (string, []any) {
	if len(r.Types) == 0 {
		return "", nil
	}
	args := []any{key}
	ph := make([]string, len(r.Types))
	for i, t := range r.Types {
		ph[i] = "?"
		args = append(args, t)
	}
	// Keyed on key alone, matching ActivityFor — an observer that is also a
	// repeater must see its own relayed traffic.
	q := `SELECT MAX(last_at) FROM target_activity
	      WHERE key = ? AND payload_type IN (` + strings.Join(ph, ",") + `)`
	if r.Direction == DirSent || r.Direction == DirCarried {
		q += ` AND direction = ?`
		args = append(args, string(r.Direction))
	}
	return q, args
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

// SetWatchRecovery chooses whether this watch announces recoveries.
func (s *Store) SetWatchRecovery(ctx context.Context, userID, watchID int64, on bool) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE watches SET notify_recovery = ? WHERE id = ? AND user_id = ?`,
			boolInt(on), watchID, userID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// SetWatchMute silences a watch until a time, without stopping it tracking
// reality — a muted watch still moves through its states, it just doesn't
// shout, which is what someone doing maintenance wants.
func (s *Store) SetWatchMute(ctx context.Context, userID, watchID int64, mutedUntil time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE watches SET muted_until = ? WHERE id = ? AND user_id = ?`,
			nullInt(mutedUntil), watchID, userID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// ---------------------------------------------------------------- rules --

// ErrRuleLimit is returned when a watch already has as many rules as it may.
var ErrRuleLimit = errors.New("store: rule limit reached")

// MaxRulesPerWatch bounds how many conditions one watch may carry. Generous
// for any real use; it exists so a scripted client cannot make one watch cost
// unbounded work on every poll.
const MaxRulesPerWatch = 12

// WatchOwnedBy confirms a watch belongs to a user before anything is changed
// through it, so a rule endpoint cannot be pointed at someone else's watch.
func (s *Store) WatchOwnedBy(ctx context.Context, userID, watchID int64) (Watch, error) {
	row := s.read.QueryRowContext(ctx,
		`SELECT `+watchCols+` FROM watches WHERE id = ? AND user_id = ?`, watchID, userID)
	w, err := scanWatch(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Watch{}, ErrNotFound
	}
	return w, err
}

// AddRule appends a rule to a watch and seeds its state.
func (s *Store) AddRule(ctx context.Context, userID int64, r Rule) (int64, error) {
	now := s.Now().UTC()
	var id int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var kind, key string
		err := tx.QueryRowContext(ctx,
			`SELECT target_kind, target_key FROM watches WHERE id = ? AND user_id = ?`,
			r.WatchID, userID).Scan(&kind, &key)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		var n int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM watch_rules WHERE watch_id = ?`, r.WatchID).Scan(&n); err != nil {
			return err
		}
		if n >= MaxRulesPerWatch {
			return ErrRuleLimit
		}
		id, err = s.insertRuleTx(ctx, tx, r, kind, key, now)
		return err
	})
	return id, err
}

// UpdateRule changes an existing rule in place, keeping its alert state.
//
// State is deliberately kept rather than reset. Widening or narrowing a rule
// does not mean the node's history stops counting, and the next poll
// re-derives everything anyway: if the new type set has no evidence the
// evaluator drops it to unknown on its own, and if the threshold moved the
// state machine picks that up the same way it picks up anything else.
// Wiping notify_count here would be actively wrong — a node already alerting
// would announce itself a second time just because someone adjusted a
// threshold.
func (s *Store) UpdateRule(ctx context.Context, userID int64, r Rule) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE watch_rules SET label = ?, types = ?, direction = ?, threshold_hours = ?
			WHERE id = ? AND watch_id IN (SELECT id FROM watches WHERE user_id = ?)`,
			r.Label, encodeTypes(r.Types), string(r.Direction), r.ThresholdHours,
			r.ID, userID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// DeleteRule removes one rule. Its state goes with it by cascade.
func (s *Store) DeleteRule(ctx context.Context, userID, ruleID int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			DELETE FROM watch_rules WHERE id = ? AND watch_id IN
				(SELECT id FROM watches WHERE user_id = ?)`, ruleID, userID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

const ruleCols = `id, watch_id, label, source, types, direction, threshold_hours, created_at`

func scanRule(row interface{ Scan(...any) error }) (Rule, error) {
	var r Rule
	var source, types, dir string
	var created int64
	if err := row.Scan(&r.ID, &r.WatchID, &r.Label, &source, &types, &dir,
		&r.ThresholdHours, &created); err != nil {
		return Rule{}, err
	}
	r.Source = Source(source)
	r.Types = decodeTypes(types)
	r.Direction = Direction(dir)
	r.CreatedAt = time.Unix(created, 0).UTC()
	return r, nil
}

// AllRules returns every rule keyed by watch id, for the evaluator.
func (s *Store) AllRules(ctx context.Context) (map[int64][]Rule, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT `+ruleCols+` FROM watch_rules ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]Rule{}
	for rows.Next() {
		r, err := scanRule(rows)
		if err != nil {
			return nil, err
		}
		out[r.WatchID] = append(out[r.WatchID], r)
	}
	return out, rows.Err()
}

// RulesForWatch returns one watch's rules in creation order.
func (s *Store) RulesForWatch(ctx context.Context, watchID int64) ([]Rule, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT `+ruleCols+` FROM watch_rules WHERE watch_id = ? ORDER BY id`, watchID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Rule
	for rows.Next() {
		r, err := scanRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ------------------------------------------------------------- activity --

// UpsertActivity records per-type evidence from one poll.
//
// last_at only ever moves forward, for the same reason target timestamps do:
// the packet feed is a sliding window that can be re-read, and letting a row
// regress would manufacture an outage out of a replayed page.
func (s *Store) UpsertActivity(ctx context.Context, tx *sql.Tx, kind, key string, payloadType int, dir Direction, at time.Time, n int) error {
	if at.IsZero() || n <= 0 {
		return nil
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO target_activity (kind, key, payload_type, direction, last_at, evidence_count)
		VALUES (?,?,?,?,?,?)
		ON CONFLICT(kind, key, payload_type, direction) DO UPDATE SET
			last_at = MAX(last_at, excluded.last_at),
			evidence_count = evidence_count + excluded.evidence_count`,
		kind, strings.ToLower(key), payloadType, string(dir), at.UTC().Unix(), n)
	return err
}

// ActivityFor returns every per-type row recorded for one key.
//
// Deliberately ignores kind. Attribution files everything under 'node',
// because a route hop is a node identity — but 7 of the 9 observers on the
// live instance ARE nodes, sharing the same 64-hex key, and several of them
// relay. Keying the lookup on kind as well would hide a repeater's own
// traffic from the observer watch on the very same box.
func (s *Store) ActivityFor(ctx context.Context, kind, key string) ([]Activity, error) {
	rows, err := s.read.QueryContext(ctx, `
		SELECT payload_type, direction, MAX(last_at), SUM(evidence_count)
		FROM target_activity WHERE key = ?
		GROUP BY payload_type, direction
		ORDER BY payload_type`, strings.ToLower(key))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Activity
	for rows.Next() {
		var a Activity
		var dir string
		var at int64
		if err := rows.Scan(&a.PayloadType, &dir, &at, &a.EvidenceCount); err != nil {
			return nil, err
		}
		a.Direction = Direction(dir)
		a.LastAt = time.Unix(at, 0).UTC()
		out = append(out, a)
	}
	return out, rows.Err()
}

// AllActivity returns every activity row keyed by target KEY, not by
// (kind, key). See ActivityFor: a route hop is always attributed to a node,
// and most observers are the same physical box as a node with the same key,
// so keying on kind would hide a repeater's traffic from the observer watch
// sitting on it.
func (s *Store) AllActivity(ctx context.Context) (map[string][]Activity, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT key, payload_type, direction, MAX(last_at), SUM(evidence_count)
		 FROM target_activity GROUP BY key, payload_type, direction`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]Activity{}
	for rows.Next() {
		var key, dir string
		var a Activity
		var at int64
		if err := rows.Scan(&key, &a.PayloadType, &dir, &at, &a.EvidenceCount); err != nil {
			return nil, err
		}
		a.Direction = Direction(dir)
		a.LastAt = time.Unix(at, 0).UTC()
		out[key] = append(out[key], a)
	}
	return out, rows.Err()
}

// KeyIsAlsoNode reports whether this key is known as a mesh node as well.
// True for 7 of the 9 observers on the live instance — the same box doing
// both jobs — and that is what decides whether per-type rules can say
// anything useful about an observer.
func (s *Store) KeyIsAlsoNode(ctx context.Context, key string) (bool, error) {
	var n int
	err := s.read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM targets WHERE kind = 'node' AND key = ?`,
		strings.ToLower(key)).Scan(&n)
	return n > 0, err
}

// Cursor is how far through the packet feed we have read. Low and High are
// the half-open range of packet ids already counted, which lets the backfill
// ingest strictly older packets without double-counting the overlap.
type Cursor struct {
	Low, High    int64
	BackfilledAt time.Time
}

// FeedCursor returns the ingest cursor.
func (s *Store) FeedCursor(ctx context.Context) (Cursor, error) {
	var c Cursor
	var backfilled int64
	err := s.read.QueryRowContext(ctx,
		`SELECT low_packet_id, last_packet_id, backfilled_at FROM feed_cursor WHERE id = 1`).
		Scan(&c.Low, &c.High, &backfilled)
	if errors.Is(err, sql.ErrNoRows) {
		return Cursor{}, nil
	}
	if backfilled > 0 {
		c.BackfilledAt = time.Unix(backfilled, 0).UTC()
	}
	return c, err
}

// SetFeedCursor widens the consumed range inside the caller's transaction, so
// evidence and the cursor that says it was consumed commit together.
//
// High only ever moves forward and Low only ever moves back — the range of
// what has been counted can grow at either end but never shrink, so a
// re-served page can't be counted twice.
func (s *Store) SetFeedCursor(ctx context.Context, tx *sql.Tx, low, high int64, backfilled bool) error {
	var backfilledAt any = 0
	if backfilled {
		backfilledAt = s.Now().UTC().Unix()
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO feed_cursor (id, low_packet_id, last_packet_id, updated_at, backfilled_at)
		VALUES (1, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			low_packet_id = CASE
				WHEN low_packet_id = 0 THEN excluded.low_packet_id
				WHEN excluded.low_packet_id = 0 THEN low_packet_id
				ELSE MIN(low_packet_id, excluded.low_packet_id) END,
			last_packet_id = MAX(last_packet_id, excluded.last_packet_id),
			updated_at = excluded.updated_at,
			backfilled_at = MAX(backfilled_at, excluded.backfilled_at)`,
		low, high, s.Now().UTC().Unix(), backfilledAt)
	return err
}

// encodeTypes renders a rule's payload types for storage.
func encodeTypes(types []int) string {
	if len(types) == 0 {
		return ""
	}
	parts := make([]string, len(types))
	for i, t := range types {
		parts[i] = strconv.Itoa(t)
	}
	return strings.Join(parts, ",")
}

// decodeTypes parses a stored type list, skipping anything unparseable rather
// than failing the query — one bad row should not take the dashboard down.
func decodeTypes(s string) []int {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []int
	for _, p := range strings.Split(s, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(p)); err == nil {
			out = append(out, n)
		}
	}
	return out
}

const watchCols = `id, user_id, target_kind, target_key, threshold_hours,
	alert_on_relay, COALESCE(label,''), muted_until, notify_recovery, created_at`

func scanWatch(row interface{ Scan(...any) error }) (Watch, error) {
	var w Watch
	var relay, recovery int
	var muted sql.NullInt64
	var created int64
	if err := row.Scan(&w.ID, &w.UserID, &w.TargetKind, &w.TargetKey, &w.ThresholdHours,
		&relay, &w.Label, &muted, &recovery, &created); err != nil {
		return Watch{}, err
	}
	w.AlertOnRelay = relay != 0
	w.NotifyRecovery = recovery != 0
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

const stateCols = `watch_id, rule_id, state, since, consecutive, observed_at,
	alerting_since, last_notified_at, notify_count, seeded`

func scanState(row interface{ Scan(...any) error }) (WatchState, error) {
	var st WatchState
	var state string
	var since int64
	var observed, alerting, notified sql.NullInt64
	var seeded int
	if err := row.Scan(&st.WatchID, &st.RuleID, &state, &since, &st.Consecutive,
		&observed, &alerting, &notified, &st.NotifyCount, &seeded); err != nil {
		return WatchState{}, err
	}
	st.State = State(state)
	st.Since = time.Unix(since, 0).UTC()
	st.ObservedAt = timeFrom(observed)
	st.AlertingSince = timeFrom(alerting)
	st.LastNotified = timeFrom(notified)
	st.Seeded = seeded != 0
	return st, nil
}

// AllWatchState returns every state row keyed by watch id and rule id.
func (s *Store) AllWatchState(ctx context.Context) (map[int64]map[int64]WatchState, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT `+stateCols+` FROM watch_state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]map[int64]WatchState{}
	for rows.Next() {
		st, err := scanState(rows)
		if err != nil {
			return nil, err
		}
		if out[st.WatchID] == nil {
			out[st.WatchID] = map[int64]WatchState{}
		}
		out[st.WatchID][st.RuleID] = st
	}
	return out, rows.Err()
}

// SaveWatchState persists one evaluated state row.
func (s *Store) SaveWatchState(ctx context.Context, tx *sql.Tx, st WatchState) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO watch_state (watch_id, rule_id, state, since, consecutive,
			observed_at, alerting_since, last_notified_at, notify_count, seeded)
		VALUES (?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(watch_id, rule_id) DO UPDATE SET
			state = excluded.state, since = excluded.since,
			consecutive = excluded.consecutive, observed_at = excluded.observed_at,
			alerting_since = excluded.alerting_since,
			last_notified_at = excluded.last_notified_at,
			notify_count = excluded.notify_count, seeded = excluded.seeded`,
		st.WatchID, st.RuleID, string(st.State), st.Since.UTC().Unix(),
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

// Retention windows for the three tables that grow with time rather than with
// the size of the mesh.
//
// Everything else here is naturally bounded: targets and target_activity have
// one row per node (and per payload type), so they stop growing when the mesh
// does. No packet is ever stored — only the timestamp of the most recent one
// of each kind. These three are the exceptions, and a poll every five minutes
// is about 105,000 poll_runs a year if nothing removes them.
const (
	// pollRunRetention outlives every window that reads this table:
	// MaxRecentNodeCount looks back 7 days, ConsecutiveNonAdvancingPolls 20
	// rows. A month leaves plenty of room to look into "why didn't I get an
	// alert last week".
	pollRunRetention = 30 * 24 * time.Hour
	// notificationRetention applies only to messages already delivered or
	// abandoned. Anything still queued is kept regardless of age.
	notificationRetention = 90 * 24 * time.Hour
	// alertEventRetention is the "you never told me" audit trail. It only
	// gains a row on an actual state change, so a year of it is small.
	alertEventRetention = 365 * 24 * time.Hour
)

// Prune drops history past its retention window. Returns how many rows went.
func (s *Store) Prune(ctx context.Context) (int64, error) {
	now := s.Now().UTC()
	var total int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		for _, q := range []struct {
			stmt   string
			cutoff time.Time
		}{
			{`DELETE FROM poll_runs WHERE started_at < ?`, now.Add(-pollRunRetention)},
			{`DELETE FROM notifications WHERE sent_at IS NOT NULL AND sent_at < ?`, now.Add(-notificationRetention)},
			{`DELETE FROM alert_events WHERE at < ?`, now.Add(-alertEventRetention)},
		} {
			res, err := tx.ExecContext(ctx, q.stmt, q.cutoff.Unix())
			if err != nil {
				return err
			}
			n, _ := res.RowsAffected()
			total += n
		}
		return nil
	})
	return total, err
}

// LastOKPollAt is when the feed was last read successfully, or zero if it
// never has been. Asked directly rather than scanned out of a page of recent
// runs, so a long outage cannot make a healthy history look like "never".
func (s *Store) LastOKPollAt(ctx context.Context) (time.Time, error) {
	var at sql.NullInt64
	err := s.read.QueryRowContext(ctx,
		`SELECT MAX(started_at) FROM poll_runs WHERE status = 'ok'`).Scan(&at)
	if err != nil {
		return time.Time{}, err
	}
	return timeFrom(at), nil
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

func (s *Store) RecordAlertEvent(ctx context.Context, tx *sql.Tx, watchID, ruleID int64, label string, from, to State, pollRunID int64, notified bool) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO alert_events (watch_id, rule_id, signal, from_state, to_state, at, poll_run_id, notified)
		 VALUES (?,?,?,?,?,?,?,?)`,
		watchID, ruleID, label, string(from), string(to),
		s.Now().UTC().Unix(), pollRunID, boolInt(notified))
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
	rules, err := s.AllRules(ctx)
	if err != nil {
		return nil, err
	}
	for i := range views {
		w := views[i].Watch
		byRule := states[w.ID]
		for _, r := range rules[w.ID] {
			views[i].Rules = append(views[i].Rules, RuleView{Rule: r, State: byRule[r.ID]})
		}
		t, err := s.Target(ctx, w.TargetKind, w.TargetKey)
		if err == nil {
			tCopy := t
			views[i].Target = &tCopy
		} else if !errors.Is(err, ErrNotFound) {
			return nil, err
		}
	}
	return views, nil
}

// WatchViewByID returns one watch's full view, for the detail page.
func (s *Store) WatchViewByID(ctx context.Context, userID, watchID int64) (WatchView, error) {
	w, err := s.WatchOwnedBy(ctx, userID, watchID)
	if err != nil {
		return WatchView{}, err
	}
	v := WatchView{Watch: w}

	rules, err := s.RulesForWatch(ctx, watchID)
	if err != nil {
		return WatchView{}, err
	}
	states, err := s.AllWatchState(ctx)
	if err != nil {
		return WatchView{}, err
	}
	for _, r := range rules {
		v.Rules = append(v.Rules, RuleView{Rule: r, State: states[watchID][r.ID]})
	}

	t, err := s.Target(ctx, w.TargetKind, w.TargetKey)
	if err == nil {
		tCopy := t
		v.Target = &tCopy
	} else if !errors.Is(err, ErrNotFound) {
		return WatchView{}, err
	}

	if w.TargetKind == string(corescope.KindObserver) {
		if also, err := s.KeyIsAlsoNode(ctx, w.TargetKey); err == nil {
			v.AlsoNode = also
		} else {
			return WatchView{}, err
		}
	}
	return v, nil
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

// ------------------------------------------------------------- status ----

// MarkCoreRepeaters stamps every repeater whose current scores qualify it as
// backbone.
//
// Kept separate from UpsertTargets because it is a judgement using thresholds
// the store has no business knowing. Called once per poll, right after the
// scores are written.
func (s *Store) MarkCoreRepeaters(ctx context.Context, minBridge, minTraffic float64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE targets SET last_core_at = ?
			 WHERE kind = 'node' AND (bridge_score >= ? OR traffic_share >= ?)`,
			s.Now().UTC().Unix(), minBridge, minTraffic)
		return err
	})
}

// StatusTarget is one repeater as the public board sees it: where it is, and
// when it was last seen doing each of the two things the board reports on.
type StatusTarget struct {
	Kind string
	Key  string
	Name string
	Role string
	Lat  *float64
	Lon  *float64
	// BridgeScore and TrafficShare are CoreScope's structural-importance
	// measures, used to pick out the backbone.
	BridgeScore  float64
	TrafficShare float64
	// LastCoreAt is when the scores last said backbone. Zero means never.
	LastCoreAt time.Time

	// LastSeen is CoreScope's own figure, used only for ordering and for the
	// "never heard of it" case.
	LastSeen time.Time
	// LastStandard is the newest evidence of ordinary traffic — the types a
	// repeater either sends itself or genuinely carries.
	LastStandard time.Time
	// LastAdvert is the newest advert, sent or carried. Sparser than traffic
	// but impossible to fake: an advert names its sender outright.
	LastAdvert time.Time
	// LastPacket is what an observer last heard, as opposed to LastSeen which
	// is when it last checked in. Zero for nodes.
	LastPacket time.Time
}

// StatusTargets returns every repeater with its two freshness figures, in one
// pass rather than a query per node.
//
// standardTypes is the payload-type set the "traffic" column judges. Passed
// in rather than hardcoded so the board and the Standard rule template cannot
// drift apart.
func (s *Store) StatusTargets(ctx context.Context, standardTypes []int) ([]StatusTarget, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT kind, key, name, role, lat, lon, last_seen_at, last_packet_at,
		        bridge_score, traffic_share, last_core_at
		 FROM targets
		 WHERE (kind = 'node' AND role IN ('repeater','room')) OR kind = 'observer'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []StatusTarget
	byKey := map[string]int{}
	for rows.Next() {
		var t StatusTarget
		var seen, packet, core sql.NullInt64
		var bridge, traffic sql.NullFloat64
		if err := rows.Scan(&t.Kind, &t.Key, &t.Name, &t.Role, &t.Lat, &t.Lon,
			&seen, &packet, &bridge, &traffic, &core); err != nil {
			return nil, err
		}
		t.LastSeen = timeFrom(seen)
		t.LastPacket = timeFrom(packet)
		t.BridgeScore, t.TrafficShare = bridge.Float64, traffic.Float64
		t.LastCoreAt = timeFrom(core)
		// Observers and nodes can share a key; the board keeps them apart, so
		// the activity index is keyed by both.
		byKey[t.Kind+"|"+t.Key] = len(out)
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}

	std := make(map[int]bool, len(standardTypes))
	for _, ty := range standardTypes {
		std[ty] = true
	}

	// One sweep of the activity table. Attribution files everything under
	// 'node', and this only ever asks about nodes, so no kind filter is
	// needed — see ActivityFor for why kind is not part of the key.
	act, err := s.read.QueryContext(ctx,
		`SELECT key, payload_type, MAX(last_at) FROM target_activity GROUP BY key, payload_type`)
	if err != nil {
		return nil, err
	}
	defer act.Close()
	for act.Next() {
		var key string
		var ty int
		var at int64
		if err := act.Scan(&key, &ty, &at); err != nil {
			return nil, err
		}
		// Attribution files everything under 'node'. An observer sharing a
		// key with a node is the same box, so it inherits that evidence.
		for _, kind := range []string{"node", "observer"} {
			i, ok := byKey[kind+"|"+key]
			if !ok {
				continue
			}
			when := time.Unix(at, 0).UTC()
			if std[ty] && when.After(out[i].LastStandard) {
				out[i].LastStandard = when
			}
			if ty == advertPayloadType && when.After(out[i].LastAdvert) {
				out[i].LastAdvert = when
			}
		}
	}
	return out, act.Err()
}

// advertPayloadType is ADVERT. Spelled out here rather than importing the
// corescope constant, to keep the store free of that dependency.
const advertPayloadType = 4
