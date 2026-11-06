package poller

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"hopreact/internal/alert"
	"hopreact/internal/config"
	"hopreact/internal/corescope"
	"hopreact/internal/store"
)

// batteryDays is how much history each battery fetch asks for: a week, which
// is what the trend and the chart in messages both use.
const batteryDays = 7

// batteryFetchers is how many battery reports are fetched at once.
const batteryFetchers = 4

// Notification kinds for battery messages.
const (
	KindBattery        = "battery"
	KindBatteryEnabled = "battery_enabled"
)

// BatteryPayload is one battery alert. It carries the week of readings so the
// message can draw its chart and trend as they were when the alert fired.
type BatteryPayload struct {
	Kind       string `json:"kind"`
	Level      string `json:"level"` // low, critical, overcharged or recovered
	TargetKey  string `json:"target_key"`
	TargetName string `json:"target_name"`
	MV         int    `json:"mv"`
	ReadUnix   int64  `json:"read_unix"`
	Reporter   string `json:"reporter"`
	// FiredUnix is when the alert was decided, so a message retried hours
	// later still draws its chart and trend as of then.
	FiredUnix    int64      `json:"fired_unix"`
	LowMV        int        `json:"low_mv"`
	CriticalMV   int        `json:"critical_mv"`
	OverchargeMV int        `json:"overcharge_mv"`
	Samples      [][2]int64 `json:"samples"`
}

// BatteryEnabledItem is one repeater in a "battery alerts are now on" message.
type BatteryEnabledItem struct {
	Name  string `json:"name"`
	MV    int    `json:"mv"`
	Level string `json:"level"`
}

// BatteryEnabledPayload tells someone battery rules were added for them.
type BatteryEnabledPayload struct {
	Kind       string               `json:"kind"`
	Repeaters  []BatteryEnabledItem `json:"repeaters"`
	LowMV      int                  `json:"low_mv"`
	CriticalMV int                  `json:"critical_mv"`
}

// batteryDue reports whether it is time to fetch battery readings again.
// Readings arrive every hour or two, so this runs less often than the poll.
func (p *Poller) batteryDue(now time.Time) bool {
	return p.Cfg.Battery.Enabled &&
		(p.batteryAt.IsZero() || now.Sub(p.batteryAt) >= p.Cfg.Battery.CheckEvery)
}

// BatteryLevelsFor resolves the levels one rule judges against.
func BatteryLevelsFor(r store.BatteryRule, cfg config.BatteryConfig, rd store.BatteryReadings) alert.BatteryLevels {
	return alert.ResolveBatteryLevels(r.LowMV, r.CriticalMV, r.OverchargeOn, r.OverchargeMV,
		cfg.LowMV, cfg.CriticalMV, cfg.OverchargeMV, rd.CSLowMV, rd.CSCriticalMV)
}

func batteryState(r store.BatteryRule) alert.BatteryState {
	return alert.BatteryState{
		Level: alert.BatteryLevel(r.Level), Since: r.Since,
		Candidate: alert.BatteryLevel(r.Candidate), CandidateCount: r.CandidateCount,
		LastReadingAt: r.LastReadingAt, LastMV: r.LastMV,
		Announced: alert.BatteryLevel(r.Announced),
	}
}

func withBatteryState(r store.BatteryRule, st alert.BatteryState) store.BatteryRule {
	r.Level, r.Since = string(st.Level), st.Since
	r.Candidate, r.CandidateCount = string(st.Candidate), st.CandidateCount
	r.LastReadingAt, r.LastMV = st.LastReadingAt, st.LastMV
	r.Announced = string(st.Announced)
	return r
}

// checkBatteries fetches battery readings for watched repeaters, adds battery
// rules where they are due, and judges the rules that exist.
//
// Like evaluate, it only ever runs inside a poll whose feed was believed, and
// it writes everything for one check in a single transaction, so a message is
// only queued if the state that justified it was saved.
func (p *Poller) checkBatteries(ctx context.Context, now time.Time, snap corescope.Snapshot) error {
	p.batteryAt = now
	bc := p.Cfg.Battery

	watches, err := p.Store.ActiveWatches(ctx)
	if err != nil {
		return err
	}
	rules, err := p.Store.BatteryRules(ctx)
	if err != nil {
		return err
	}
	users, err := p.Store.UsersWithWatches(ctx)
	if err != nil {
		return err
	}
	auto := map[int64]bool{}
	for _, u := range users {
		auto[u.ID] = u.AutoBattery
	}
	names := map[string]string{}
	for _, n := range snap.Nodes {
		names[n.Key] = n.Name
	}

	// A repeater is fetched if someone has a battery rule on it, or might be
	// given one.
	need := map[string]bool{}
	for _, w := range watches {
		if w.TargetKind != string(corescope.KindNode) {
			continue
		}
		if _, has := rules[w.ID]; has || (w.BatteryAddedAt.IsZero() && auto[w.UserID]) {
			need[w.TargetKey] = true
		}
	}
	keys := make([]string, 0, len(need))
	for k := range need {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// Fetched a few at a time, so a slow CoreScope can't hold up the poll
	// loop for one full timeout per repeater.
	type fetched struct {
		key string
		rep corescope.BatteryReport
		err error
	}
	results := make([]fetched, len(keys))
	sem := make(chan struct{}, batteryFetchers)
	var wg sync.WaitGroup
	for i, key := range keys {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, key string) {
			defer wg.Done()
			defer func() { <-sem }()
			fctx, cancel := context.WithTimeout(ctx, p.Cfg.RequestTimeout())
			defer cancel()
			rep, err := p.Scope.Battery(fctx, key, batteryDays)
			results[i] = fetched{key: key, rep: rep, err: err}
		}(i, key)
	}
	wg.Wait()

	reports := map[string]store.BatteryReadings{}
	failed := 0
	for _, f := range results {
		if errors.Is(f.err, corescope.ErrNoBattery) {
			// Nobody has reported this repeater's battery. Nothing to judge,
			// and nothing to show.
			continue
		}
		if f.err != nil {
			// A failed fetch is unknown, never low: the rule keeps its state
			// until a real reading arrives.
			failed++
			continue
		}
		rep := f.rep
		rd := store.BatteryReadings{
			Key: f.key, Status: rep.Status, LatestMV: rep.LatestMV, LatestAt: rep.LatestAt,
			Reporter: rep.Reporter, Stale: rep.Stale,
			CSLowMV: rep.LowMV, CSCriticalMV: rep.CriticalMV,
			Samples: rep.Samples, FetchedAt: now,
		}
		if err := p.Store.SaveBatteryReadings(ctx, rd); err != nil {
			return err
		}
		reports[f.key] = rd
	}
	if err := p.Store.PruneBatteryReadings(ctx); err != nil {
		p.Log.Warn("could not prune battery readings", "err", err)
	}

	pol := alert.BatteryPolicy{ConfirmReadings: bc.ConfirmReadings, RecoverMarginMV: bc.RecoverMarginMV}
	type planned struct {
		watch store.Watch
		// old is the rule as read before the fetches. The save only lands if
		// the rule still looks like this, so a change its owner made during
		// the check wins and no message goes out for a rule they just
		// switched off.
		old  store.BatteryRule
		rule store.BatteryRule
		out  alert.BatteryOutcome
		lv   alert.BatteryLevels
		rd   store.BatteryReadings
	}
	type addition struct {
		rule   store.BatteryRule
		userID int64
		item   BatteryEnabledItem
		lv     alert.BatteryLevels
		muted  bool
	}
	var plans []planned
	var adds []addition
	entering := 0
	inverted := 0

	for _, w := range watches {
		rd, ok := reports[w.TargetKey]
		if !ok || w.TargetKind != string(corescope.KindNode) {
			continue
		}
		muted := !w.MutedUntil.IsZero() && now.Before(w.MutedUntil)
		rule, has := rules[w.ID]

		if !has {
			// A stale reading could call a battery "already low" from days
			// ago, so the rule waits for a fresh one.
			if !w.BatteryAddedAt.IsZero() || !auto[w.UserID] || !rd.Status.HasBattery() || rd.Stale {
				continue
			}
			// Add the rule already at the level the battery is at now. If it is
			// low already, that is said once in the switch-on message rather
			// than as a separate alert.
			lv := BatteryLevelsFor(store.BatteryRule{}, bc, rd)
			level := alert.ClassifyBattery(rd.LatestMV, alert.BatteryUnknown, lv, bc.RecoverMarginMV)
			adds = append(adds, addition{
				rule: store.BatteryRule{
					WatchID: w.ID, Level: string(level), Since: rd.LatestAt,
					LastReadingAt: rd.LatestAt, LastMV: rd.LatestMV,
				},
				userID: w.UserID, lv: lv, muted: muted,
				item: BatteryEnabledItem{Name: displayName(names[w.TargetKey], w), MV: rd.LatestMV, Level: string(level)},
			})
			continue
		}

		// CoreScope's "unknown" is no reading in the window, which says nothing
		// about the battery. Only 0 mV ("no_battery") means none is fitted.
		if !rd.Status.HasBattery() && rd.Status != corescope.BatteryNone {
			continue
		}
		lv := BatteryLevelsFor(rule, bc, rd)
		if lv.CriticalMV >= lv.LowMV {
			// Possible when a level follows a CoreScope configured differently
			// from its defaults. Judged as it stands, but worth a line in the log.
			inverted++
		}
		reading := alert.BatteryReading{MV: rd.LatestMV, At: rd.LatestAt, HasBattery: rd.Status.HasBattery()}
		out := alert.EvaluateBattery(reading, batteryState(rule), lv, pol,
			alert.Prefs{Muted: muted, NotifyRecovery: w.NotifyRecovery})
		if !out.Changed {
			continue
		}
		if out.Notify && out.Kind != "recovered" {
			entering++
		}
		plans = append(plans, planned{watch: w, old: rule, rule: withBatteryState(rule, out.State), out: out, lv: lv, rd: rd})
	}
	if inverted > 0 {
		p.Log.Warn("battery levels resolve to critical at or above low; check battery.low_mv/critical_mv against CoreScope's thresholds",
			"rules", inverted)
	}

	// The same backstop as the quiet-node alerts: a burst of battery alerts in
	// one check is likelier to be a CoreScope fault than that many batteries
	// failing together. Only the absolute limit applies: with a handful of
	// battery rules, a fraction would trip on two genuine alerts.
	//
	// When it trips, the new levels are still saved but nobody is told, and
	// the operator gets one message to go and look. Holding the levels back
	// instead would judge the same readings again and trip every check. That
	// many batteries crossing in one check is a CoreScope fault, such as a
	// threshold or calibration change, far more often than a real event, and
	// the watch pages still show every battery's true level.
	if entering > p.Cfg.Alerts.MaxNewAlertsPerPoll {
		reason := fmt.Sprintf("battery check: %d repeaters went low at once (limit %d)",
			entering, p.Cfg.Alerts.MaxNewAlertsPerPoll)
		p.Log.Error("battery alert breaker tripped — no battery alerts sent", "reason", reason)
		if p.OnBreaker != nil {
			p.OnBreaker(ctx, reason)
		}
		for i := range plans {
			pl := &plans[i]
			if pl.out.Notify && pl.out.Kind != "recovered" {
				// Never announced, so its recovery stays quiet too.
				pl.out.Notify = false
				pl.rule.Announced = pl.old.Announced
			}
		}
	}

	queued, switchOns := 0, 0
	err = p.Store.WriteTx(ctx, func(tx *sql.Tx) error {
		enabled := map[int64]*BatteryEnabledPayload{}
		var enabledOrder []int64
		for _, a := range adds {
			// Re-checked inside the transaction: the owner may have turned
			// automatic adding off, or made their own choice, during the check.
			inserted, err := p.Store.InsertAutoBatteryRuleTx(ctx, tx, a.rule, now)
			if err != nil {
				return err
			}
			if !inserted || a.muted {
				continue
			}
			m := enabled[a.userID]
			if m == nil {
				m = &BatteryEnabledPayload{Kind: KindBatteryEnabled, LowMV: a.lv.LowMV, CriticalMV: a.lv.CriticalMV}
				enabled[a.userID] = m
				enabledOrder = append(enabledOrder, a.userID)
			}
			m.Repeaters = append(m.Repeaters, a.item)
		}
		for _, pl := range plans {
			saved, err := p.Store.SaveBatteryStateTx(ctx, tx, pl.old, pl.rule)
			if err != nil {
				return err
			}
			if !saved {
				continue // changed or removed by its owner during the check
			}
			if pl.out.State.Level != pl.out.From {
				if err := p.Store.RecordAlertEvent(ctx, tx, pl.watch.ID, 0, "battery",
					store.State(pl.out.From), store.State(pl.out.State.Level), 0, pl.out.Notify); err != nil {
					return err
				}
			}
			if !pl.out.Notify {
				continue
			}
			payload, err := json.Marshal(BatteryPayload{
				Kind: KindBattery, Level: pl.out.Kind,
				TargetKey: pl.watch.TargetKey, TargetName: displayName(names[pl.watch.TargetKey], pl.watch),
				MV: pl.rd.LatestMV, ReadUnix: unixOrZero(pl.rd.LatestAt), Reporter: pl.rd.Reporter,
				FiredUnix: now.Unix(),
				LowMV:     pl.lv.LowMV, CriticalMV: pl.lv.CriticalMV, OverchargeMV: pl.lv.OverchargeMV,
				Samples: corescope.PackSamples(pl.rd.Samples),
			})
			if err != nil {
				return err
			}
			if err := p.Store.QueueNotification(ctx, tx, pl.watch.UserID, KindBattery, string(payload), time.Time{}); err != nil {
				return err
			}
			queued++
		}
		for _, uid := range enabledOrder {
			m := enabled[uid]
			payload, err := json.Marshal(m)
			if err != nil {
				return err
			}
			if err := p.Store.QueueNotification(ctx, tx, uid, KindBatteryEnabled, string(payload), time.Time{}); err != nil {
				return err
			}
			if err := p.Store.SetBatteryBannerTx(ctx, tx, uid, len(m.Repeaters)); err != nil {
				return err
			}
			switchOns++
		}
		return nil
	})
	if err != nil {
		return err
	}
	p.Log.Info("battery check complete", "repeaters", len(keys), "with_readings", len(reports),
		"failed", failed, "rules_added", len(adds), "alerts", queued, "switch_on_messages", switchOns)
	return nil
}

func displayName(name string, w store.Watch) string {
	if name != "" {
		return name
	}
	if w.Label != "" {
		return w.Label
	}
	if len(w.TargetKey) > 12 {
		return w.TargetKey[:12] + "…"
	}
	return w.TargetKey
}
