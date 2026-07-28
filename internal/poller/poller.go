// Package poller runs the loop: fetch CoreScope, judge whether to believe
// it, evaluate every watch, and queue whatever needs saying.
//
// Two structural rules do most of the safety work here:
//
//  1. Evaluation only ever happens as a step INSIDE a healthy poll. It is
//     never on its own timer. That single rule removes the entire class of
//     "HopReact was down for twelve hours, so every stored timestamp is
//     stale and everything looks offline" — the data being evaluated was
//     always fetched moments ago.
//
//  2. Nothing is sent from here. Transitions queue messages in the same
//     transaction that commits their state, and a separate drainer sends
//     them. So a message can only exist if the state change that justified
//     it also committed, and a crash loses at most one message rather than
//     re-sending a storm.
package poller

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"hopreact/internal/alert"
	"hopreact/internal/attribute"
	"hopreact/internal/config"
	"hopreact/internal/corescope"
	"hopreact/internal/store"
)

// Poller owns one polling loop.
type Poller struct {
	Store *store.Store
	Scope *corescope.Client
	Cfg   config.Config
	Log   *slog.Logger
	Now   store.Clock
	// OnBreaker is called when the blast-radius breaker trips, so the
	// operator can be told through whatever channel is configured. Optional.
	OnBreaker func(ctx context.Context, reason string)
}

// RuleTrip is one rule's contribution to a message.
type RuleTrip struct {
	Label          string `json:"label"`
	ThresholdHours int    `json:"threshold_hours"`
	LastSeenUnix   int64  `json:"last_seen_unix"`
}

// AlertPayload is the JSON body of a queued notification. Kept structured
// rather than pre-rendered so the message wording can change without a
// migration, and so the drainer can batch several into one DM.
//
// One payload covers one watch and one direction of change. A watch with
// three rules that all trip in the same poll produces a single payload
// listing three rules, not three payloads — otherwise adding rules would make
// the tool noisier, which is the opposite of what they are for.
type AlertPayload struct {
	Kind       string     `json:"kind"`
	TargetKind string     `json:"target_kind"`
	TargetKey  string     `json:"target_key"`
	TargetName string     `json:"target_name"`
	Rules      []RuleTrip `json:"rules"`
}

// Run polls on every tick until ctx is cancelled.
//
// tick is injected rather than created here so tests can drive the loop
// deterministically instead of sleeping. One poll runs immediately: a
// time.Ticker does not fire at t=0, and without this the dashboard would sit
// empty for a whole interval after every deploy.
func (p *Poller) Run(ctx context.Context, tick <-chan time.Time) {
	if err := p.PollOnce(ctx); err != nil {
		p.Log.Error("initial poll failed", "err", err)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
			if err := p.PollOnce(ctx); err != nil {
				p.Log.Error("poll failed", "err", err)
			}
		}
	}
}

// PollOnce performs one complete cycle.
func (p *Poller) PollOnce(ctx context.Context) error {
	now := p.now()
	runID, err := p.Store.StartPollRun(ctx)
	if err != nil {
		return fmt.Errorf("poller: starting run: %w", err)
	}
	run := store.PollRun{ID: runID, StartedAt: now}

	fetchCtx, cancel := context.WithTimeout(ctx, p.Cfg.RequestTimeout()*3)
	defer cancel()
	snap, fetchErr := p.Scope.Fetch(fetchCtx, now)
	run.NodeCount = len(snap.Nodes)
	run.ObserverCount = len(snap.Observers)

	// Write what we got before judging it. Even a poll we decline to
	// evaluate is useful for the dashboard's "last seen" column, and
	// UpsertTargets is monotonic so a stale view cannot damage anything.
	var advanced int
	if fetchErr == nil {
		advanced, err = p.Store.UpsertTargets(ctx, snap.All())
		if err != nil {
			return fmt.Errorf("poller: storing targets: %w", err)
		}
	}
	run.AdvancedCount = advanced

	// Per-type evidence. Best-effort on purpose: if the packet feed is
	// unavailable, the seen and relayed signals are still perfectly good, and
	// per-type rules should degrade to "no fresh evidence" — which the
	// evaluator treats as unknown — rather than to "everything is offline".
	if fetchErr == nil {
		if n, err := p.ingest(ctx, snap); err != nil {
			p.Log.Warn("per-type evidence not updated this poll", "run", runID, "err", err)
		} else if n > 0 {
			p.Log.Debug("ingested packets", "run", runID, "packets", n)
		}
	}

	maxRecent, err := p.Store.MaxRecentNodeCount(ctx, now.Add(-7*24*time.Hour))
	if err != nil {
		return fmt.Errorf("poller: reading recent node counts: %w", err)
	}
	nonAdvancing, err := p.Store.ConsecutiveNonAdvancingPolls(ctx)
	if err != nil {
		return fmt.Errorf("poller: reading poll history: %w", err)
	}

	verdict := alert.JudgeFeed(alert.FeedInputs{
		FetchErr:                fetchErr,
		NodeCount:               len(snap.Nodes),
		MaxRecentNodeCount:      maxRecent,
		Advanced:                advanced,
		ConsecutiveNonAdvancing: nonAdvancing,
		MinNodes:                p.Cfg.Poll.MinNodes,
		MinNodeFraction:         p.Cfg.Poll.MinNodeFraction,
		MaxPollsWithoutAdvance:  p.Cfg.Poll.MaxPollsWithoutAdvance,
		HaveBaseline:            maxRecent > 0,
	})
	run.Status = verdict.Status

	if verdict.Status != store.PollOK {
		// The important branch. A poll we do not believe leaves the previous
		// state entirely alone — "no data" is never read as "everything is
		// offline".
		run.SuppressedReason = verdict.Reason
		if fetchErr != nil {
			run.Error = fetchErr.Error()
		}
		p.Log.Warn("poll not evaluated", "run", runID, "status", verdict.Status, "reason", verdict.Reason)
		return p.Store.FinishPollRun(ctx, run)
	}

	evaluated, err := p.evaluate(ctx, now, snap, runID)
	if err != nil {
		run.Error = err.Error()
		return p.Store.FinishPollRun(ctx, run)
	}
	run.Evaluated = evaluated.committed
	if evaluated.breakerReason != "" {
		run.SuppressedReason = evaluated.breakerReason
	}
	p.Log.Info("poll complete", "run", runID, "nodes", run.NodeCount,
		"observers", run.ObserverCount, "advanced", advanced,
		"notified", evaluated.notified, "evaluated", run.Evaluated)
	return p.Store.FinishPollRun(ctx, run)
}

// ingest reads the packet feed and records which nodes it proves were
// carrying which kinds of traffic.
//
// The cursor matters more than it looks. The feed is a sliding window that
// overlaps heavily between polls — at five packets a minute, a 600-packet
// page covers about two hours — so without a high-water mark every poll would
// re-count the same packets and evidence_count would become a measure of how
// often we polled rather than of how much was actually seen.
func (p *Poller) ingest(ctx context.Context, snap corescope.Snapshot) (int, error) {
	cursor, err := p.Store.FeedCursor(ctx)
	if err != nil {
		return 0, err
	}

	fetchCtx, cancel := context.WithTimeout(ctx, p.Cfg.RequestTimeout())
	defer cancel()
	packets, err := p.Scope.FetchPackets(fetchCtx, p.Cfg.Poll.PacketLimit)
	if err != nil {
		return 0, err
	}

	idx := attribute.BuildIndex(snap.Nodes)

	// Fold the page into one row per (node, type, direction) before touching
	// the database: a busy repeater appears in hundreds of these.
	type slot struct {
		kind string
		key  string
		typ  int
		dir  attribute.Direction
	}
	type agg struct {
		at time.Time
		n  int
	}
	rows := map[slot]*agg{}

	var highest int64
	fresh := 0
	for _, pk := range packets {
		if pk.ID > highest {
			highest = pk.ID
		}
		if pk.ID <= cursor || pk.At.IsZero() {
			continue
		}
		fresh++
		for _, h := range attribute.Attribute(pk, idx) {
			s := slot{kind: string(corescope.KindNode), key: h.Key, typ: h.Type, dir: h.Direction}
			a := rows[s]
			if a == nil {
				a = &agg{}
				rows[s] = a
			}
			a.n++
			if pk.At.After(a.at) {
				a.at = pk.At
			}
		}
	}

	if len(rows) == 0 && highest <= cursor {
		return 0, nil
	}

	err = p.Store.WriteTx(ctx, func(tx *sql.Tx) error {
		for s, a := range rows {
			if err := p.Store.UpsertActivity(ctx, tx, s.kind, s.key, s.typ,
				store.Direction(s.dir), a.at, a.n); err != nil {
				return err
			}
		}
		// Advancing the cursor in the same transaction as the evidence is
		// what makes a crash safe: either both land or neither does, so a
		// packet is never marked consumed without its evidence.
		return p.Store.SetFeedCursor(ctx, tx, highest)
	})
	return fresh, err
}

type evalResult struct {
	committed     bool
	notified      int
	breakerReason string
}

// evaluate advances every watch against the snapshot.
//
// Transitions are computed in full BEFORE anything is written, so the
// breaker can look at the whole picture and refuse the lot. Deciding
// watch-by-watch would mean the first N alerts had already gone out by the
// time the scale of the problem became apparent.
func (p *Poller) evaluate(ctx context.Context, now time.Time, snap corescope.Snapshot, runID int64) (evalResult, error) {
	var res evalResult

	watches, err := p.Store.ActiveWatches(ctx)
	if err != nil {
		return res, err
	}
	if len(watches) == 0 {
		res.committed = true
		return res, nil
	}
	states, err := p.Store.AllWatchState(ctx)
	if err != nil {
		return res, err
	}
	rules, err := p.Store.AllRules(ctx)
	if err != nil {
		return res, err
	}
	activity, err := p.Store.AllActivity(ctx)
	if err != nil {
		return res, err
	}

	// Index the snapshot for lookup.
	byKey := map[string]corescope.Observation{}
	for _, o := range snap.All() {
		byKey[string(o.Kind)+"|"+o.Key] = o
	}

	pol := alert.Policy{ConfirmPolls: p.Cfg.Alerts.ConfirmPolls}

	type pending struct {
		watch store.Watch
		rule  store.Rule
		out   alert.Outcome
	}
	var planned []pending
	entering, activeSignals := 0, 0
	watchUser := map[int64]int64{}

	for _, w := range watches {
		watchUser[w.ID] = w.UserID
		obs, known := byKey[w.TargetKind+"|"+w.TargetKey]
		muted := !w.MutedUntil.IsZero() && now.Before(w.MutedUntil)
		acts := activity[w.TargetKind+"|"+w.TargetKey]

		for _, r := range rules[w.ID] {
			activeSignals++
			cur, ok := states[w.ID][r.ID]
			if !ok {
				cur = store.WatchState{WatchID: w.ID, RuleID: r.ID, State: store.StateUnknown, Since: now}
			}
			cur.WatchID, cur.RuleID = w.ID, r.ID

			out := alert.Evaluate(now, ruleObservation(r, obs, known, acts), cur,
				time.Duration(r.ThresholdHours)*time.Hour, muted, pol)
			out.State.WatchID, out.State.RuleID = w.ID, r.ID
			if out.Entering {
				entering++
			}
			planned = append(planned, pending{watch: w, rule: r, out: out})
		}
	}

	// The catch-all for the failure nobody predicted. Several hundred
	// watches entering alert in one poll is far more likely to be our bug or
	// their outage than that many nodes genuinely failing in the same
	// instant.
	tripped, reason := alert.TripBreaker(alert.BreakerInputs{
		Entering:      entering,
		ActiveSignals: activeSignals,
		MaxPerPoll:    p.Cfg.Alerts.MaxNewAlertsPerPoll,
		MaxFraction:   p.Cfg.Alerts.MaxNewAlertFraction,
	})
	if tripped {
		res.breakerReason = reason
		p.Log.Error("alert breaker tripped — no user notifications sent", "run", runID, "reason", reason)
		if p.OnBreaker != nil {
			p.OnBreaker(ctx, reason)
		}
		return res, nil
	}

	// Messages are grouped by watch and by direction of change, so a watch
	// whose three rules all trip in one poll produces one message listing
	// three reasons. Adding rules should sharpen the alerting, not multiply
	// the pings.
	type msgKey struct {
		watchID int64
		kind    string
	}
	var msgOrder []msgKey
	msgs := map[msgKey]*AlertPayload{}

	err = p.Store.WriteTx(ctx, func(tx *sql.Tx) error {
		for _, pl := range planned {
			if !pl.out.Changed {
				continue
			}
			if err := p.Store.SaveWatchState(ctx, tx, pl.out.State); err != nil {
				return err
			}
			cur := states[pl.watch.ID][pl.rule.ID]
			if err := p.Store.RecordAlertEvent(ctx, tx, pl.watch.ID, pl.rule.ID,
				ruleLabel(pl.rule), cur.State, pl.out.State.State, runID, pl.out.Notify); err != nil {
				return err
			}
			if !pl.out.Notify {
				continue
			}
			k := msgKey{watchID: pl.watch.ID, kind: pl.out.Kind}
			m := msgs[k]
			if m == nil {
				obs := byKey[pl.watch.TargetKind+"|"+pl.watch.TargetKey]
				name := obs.Name
				if name == "" {
					name = pl.watch.Label
				}
				m = &AlertPayload{
					Kind:       pl.out.Kind,
					TargetKind: pl.watch.TargetKind,
					TargetKey:  pl.watch.TargetKey,
					TargetName: name,
				}
				msgs[k] = m
				msgOrder = append(msgOrder, k)
			}
			m.Rules = append(m.Rules, RuleTrip{
				Label:          ruleLabel(pl.rule),
				ThresholdHours: pl.rule.ThresholdHours,
				LastSeenUnix:   unixOrZero(pl.out.State.ObservedAt),
			})
		}

		for _, k := range msgOrder {
			m := msgs[k]
			payload, err := json.Marshal(m)
			if err != nil {
				return err
			}
			userID := watchUser[k.watchID]
			if err := p.Store.QueueNotification(ctx, tx, userID,
				m.Kind, string(payload), time.Time{}); err != nil {
				return err
			}
			res.notified++
		}
		return nil
	})
	if err != nil {
		return res, err
	}
	res.committed = true
	return res, nil
}

// ruleObservation turns one rule plus what we know about its target into the
// single (timestamp, applicable) pair the state machine consumes.
//
// The Applicable flag is the safety valve, and the per-type case is the one
// that needs care. Our evidence only covers adverts and path hops at least
// three bytes wide — roughly 41% of packets — so a node can be perfectly
// healthy and still produce nothing we can attribute, if its traffic happens
// to travel on narrow paths. Treating that silence as an outage would invent
// failures out of the encoding, so a rule with no evidence at all stays
// unknown and never fires. Once any evidence exists, the timestamps are
// trustworthy: a 3-byte hop is unique across every node on the mesh.
func ruleObservation(r store.Rule, obs corescope.Observation, known bool, acts []store.Activity) alert.Observation {
	switch r.Source {
	case store.SourceSeen:
		return alert.Observation{At: obs.LastSeen, Applicable: known}

	case store.SourceRelayed:
		// A node that has never relayed has not stopped relaying.
		return alert.Observation{
			At:         obs.LastRelayed,
			Applicable: known && !obs.LastRelayed.IsZero(),
		}

	case store.SourceTypes:
		want := make(map[int]bool, len(r.Types))
		for _, t := range r.Types {
			want[t] = true
		}
		var latest time.Time
		found := false
		for _, a := range acts {
			if !want[a.PayloadType] {
				continue
			}
			if r.Direction != store.DirEither && a.Direction != r.Direction {
				continue
			}
			found = true
			if a.LastAt.After(latest) {
				latest = a.LastAt
			}
		}
		return alert.Observation{At: latest, Applicable: found}
	}
	return alert.Observation{}
}

// ruleLabel is what a rule is called in a message. Falls back to describing
// the rule when the user has not named it.
func ruleLabel(r store.Rule) string {
	if strings.TrimSpace(r.Label) != "" {
		return r.Label
	}
	switch r.Source {
	case store.SourceSeen:
		return "Not heard at all"
	case store.SourceRelayed:
		return "Stopped passing traffic"
	}
	names := make([]string, 0, len(r.Types))
	for _, t := range r.Types {
		names = append(names, corescope.TypeName(t))
	}
	if len(names) == 0 {
		return "Custom rule"
	}
	return strings.Join(names, ", ")
}

func (p *Poller) now() time.Time {
	if p.Now != nil {
		return p.Now().UTC()
	}
	return time.Now().UTC()
}

func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}
