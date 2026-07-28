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
	"time"

	"hopreact/internal/alert"
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

// AlertPayload is the JSON body of a queued notification. Kept structured
// rather than pre-rendered so the message wording can change without a
// migration, and so the drainer can batch several into one DM.
type AlertPayload struct {
	Kind           string `json:"kind"`
	TargetKind     string `json:"target_kind"`
	TargetKey      string `json:"target_key"`
	TargetName     string `json:"target_name"`
	Signal         string `json:"signal"`
	ThresholdHours int    `json:"threshold_hours"`
	LastSeenUnix   int64  `json:"last_seen_unix"`
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

	// Index the snapshot for lookup.
	byKey := map[string]corescope.Observation{}
	for _, o := range snap.All() {
		byKey[string(o.Kind)+"|"+o.Key] = o
	}

	pol := alert.Policy{ConfirmPolls: p.Cfg.Alerts.ConfirmPolls}

	type pending struct {
		watch store.Watch
		out   alert.Outcome
	}
	var planned []pending
	entering, activeSignals := 0, 0

	for _, w := range watches {
		obs, known := byKey[w.TargetKind+"|"+w.TargetKey]
		muted := !w.MutedUntil.IsZero() && now.Before(w.MutedUntil)

		signals := []struct {
			sig store.Signal
			ob  alert.Observation
		}{
			{store.SignalSeen, alert.Observation{At: obs.LastSeen, Applicable: known}},
		}
		if w.AlertOnRelay {
			// Only meaningful once the target has been observed relaying at
			// least once — a node that has never relayed has not stopped.
			applicable := known && !obs.LastRelayed.IsZero()
			signals = append(signals, struct {
				sig store.Signal
				ob  alert.Observation
			}{store.SignalRelayed, alert.Observation{At: obs.LastRelayed, Applicable: applicable}})
		}

		for _, s := range signals {
			activeSignals++
			cur, ok := states[w.ID][s.sig]
			if !ok {
				cur = store.WatchState{WatchID: w.ID, Signal: s.sig, State: store.StateUnknown, Since: now}
			}
			cur.WatchID, cur.Signal = w.ID, s.sig

			out := alert.Evaluate(now, s.ob, cur,
				time.Duration(w.ThresholdHours)*time.Hour, muted, pol)
			out.State.WatchID, out.State.Signal = w.ID, s.sig
			if out.Entering {
				entering++
			}
			planned = append(planned, pending{watch: w, out: out})
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

	err = p.Store.WriteTx(ctx, func(tx *sql.Tx) error {
		for _, pl := range planned {
			if !pl.out.Changed {
				continue
			}
			if err := p.Store.SaveWatchState(ctx, tx, pl.out.State); err != nil {
				return err
			}
			cur := states[pl.watch.ID][pl.out.State.Signal]
			if err := p.Store.RecordAlertEvent(ctx, tx, pl.watch.ID, pl.out.State.Signal,
				cur.State, pl.out.State.State, runID, pl.out.Notify); err != nil {
				return err
			}
			if !pl.out.Notify {
				continue
			}
			obs := byKey[pl.watch.TargetKind+"|"+pl.watch.TargetKey]
			payload, err := json.Marshal(AlertPayload{
				Kind:           pl.out.Kind,
				TargetKind:     pl.watch.TargetKind,
				TargetKey:      pl.watch.TargetKey,
				TargetName:     obs.Name,
				Signal:         string(pl.out.State.Signal),
				ThresholdHours: pl.watch.ThresholdHours,
				LastSeenUnix:   unixOrZero(pl.out.State.ObservedAt),
			})
			if err != nil {
				return err
			}
			if err := p.Store.QueueNotification(ctx, tx, pl.watch.UserID,
				pl.out.Kind, string(payload), time.Time{}); err != nil {
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
