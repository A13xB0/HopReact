// Package alert decides who should be told what, and — more importantly —
// who should not.
//
// Evaluate is a pure function: no database, no clock, no I/O. Everything it
// needs arrives as arguments and everything it decides comes back as a
// return value, so the entire policy is table-testable without sleeping or
// standing up a store. That is deliberate; this is the part of HopReact most
// able to do damage, and the damage it can do is sending a few hundred
// people a false alarm at three in the morning.
//
// The lifecycle, per watch and per signal:
//
//	unknown ──► ok ──► pending ──► alerting ──► recovering ──► ok
//	                     │                          │
//	                     └── back under ────────────┘
//	                        (silently)
//
// Rules that matter more than the diagram:
//
//   - One notification per transition. alerting → alerting never notifies,
//     so a node down for a week produces one message, not one per poll.
//   - pending is the flap guard: a crossing must survive ConfirmPolls before
//     it becomes an alert, and falling back under the threshold in the
//     meantime is silent.
//   - A watch seeded onto an already-offline target carries NotifyCount 0,
//     which suppresses its recovery message too. You never announce recovery
//     from an alert nobody was told about.
//   - A signal with no timestamp at all stays unknown. Absence of evidence
//     is not evidence of absence: a target CoreScope has never mentioned, or
//     a node that has never relayed, has not "stopped".
package alert

import (
	"fmt"
	"time"

	"hopreact/internal/store"
)

// Policy is the tunable part of the decision.
type Policy struct {
	// ConfirmPolls is how many consecutive evaluations a crossing must
	// survive before it is believed, in each direction.
	ConfirmPolls int
}

// Observation is what one poll saw for one signal.
type Observation struct {
	// At is the signal's timestamp — when the target was last heard, or
	// last relayed. Zero means the signal has no value, which is unknown
	// rather than stale.
	At time.Time
	// Applicable is false when this signal cannot meaningfully apply: an
	// observer has no relay signal, and neither does a node that has never
	// been observed relaying.
	Applicable bool
}

// Outcome is the result of evaluating one watch signal.
type Outcome struct {
	State store.WatchState
	// Notify is set when this transition should reach the user. The
	// evaluator returning this does NOT send anything — the caller queues it
	// in the same transaction that commits State, so a message can only
	// exist if the state change that justified it also committed.
	Notify   bool
	Kind     string // "alert" or "recovered"
	Changed  bool   // state differs from the input, so it is worth persisting
	Entering bool   // this transition enters alerting; the breaker counts these
}

// Kinds of notification.
const (
	KindAlert     = "alert"
	KindRecovered = "recovered"
)

// Evaluate advances one watch signal by one poll.
//
// now is the evaluation time, obs is what the poll saw, cur is the stored
// state, threshold is the user's chosen silence, and muted suppresses
// notification (but not state movement — a muted watch still tracks reality,
// it just doesn't shout about it).
func Evaluate(now time.Time, obs Observation, cur store.WatchState, threshold time.Duration, muted bool, pol Policy) Outcome {
	confirm := pol.ConfirmPolls
	if confirm < 1 {
		confirm = 1
	}

	next := cur
	next.ObservedAt = obs.At

	// Nothing to judge. Note this deliberately does not clear NotifyCount:
	// if a target drops out of the feed mid-alert and comes back, we still
	// remember that we announced it.
	if !obs.Applicable || obs.At.IsZero() {
		if cur.State != store.StateUnknown {
			next.State = store.StateUnknown
			next.Since = now
			next.Consecutive = 0
			return Outcome{State: next, Changed: true}
		}
		return Outcome{State: next}
	}

	over := now.Sub(obs.At) >= threshold

	switch cur.State {
	case store.StateUnknown:
		// First real reading. Adopt the truth without announcing it — the
		// same reasoning as seeding a new watch: an alert for something
		// that was already broken when we started looking is noise.
		if over {
			next.State = store.StateAlerting
			next.AlertingSince = now
			next.NotifyCount = 0
			next.Seeded = true
		} else {
			next.State = store.StateOK
		}
		next.Since = now
		next.Consecutive = 0
		return Outcome{State: next, Changed: true}

	case store.StateOK:
		if !over {
			next.Consecutive = 0
			return Outcome{State: next}
		}
		next.State = store.StatePending
		next.Since = now
		next.Consecutive = 1
		if next.Consecutive >= confirm {
			return promote(next, now, muted)
		}
		return Outcome{State: next, Changed: true}

	case store.StatePending:
		if !over {
			// Came back under the threshold before we believed it. This is
			// the flap guard, and it is silent by design.
			next.State = store.StateOK
			next.Since = now
			next.Consecutive = 0
			return Outcome{State: next, Changed: true}
		}
		next.Consecutive = cur.Consecutive + 1
		if next.Consecutive >= confirm {
			return promote(next, now, muted)
		}
		return Outcome{State: next, Changed: true}

	case store.StateAlerting:
		if over {
			// Still down. Say nothing — this is the rule that keeps a
			// week-long outage to a single message.
			next.Consecutive = 0
			return Outcome{State: next}
		}
		next.State = store.StateRecovering
		next.Since = now
		next.Consecutive = 1
		if next.Consecutive >= confirm {
			return recover_(next, now, muted)
		}
		return Outcome{State: next, Changed: true}

	case store.StateRecovering:
		if over {
			// Went quiet again before recovery was confirmed. Back to
			// alerting WITHOUT re-notifying: the user was already told it
			// was down and it never stopped being down as far as they know.
			next.State = store.StateAlerting
			next.Since = now
			next.Consecutive = 0
			return Outcome{State: next, Changed: true}
		}
		next.Consecutive = cur.Consecutive + 1
		if next.Consecutive >= confirm {
			return recover_(next, now, muted)
		}
		return Outcome{State: next, Changed: true}
	}

	// Unreachable for known states; treated as unknown rather than panicking
	// on a value some future migration introduced.
	next.State = store.StateUnknown
	next.Since = now
	return Outcome{State: next, Changed: true}
}

func promote(next store.WatchState, now time.Time, muted bool) Outcome {
	next.State = store.StateAlerting
	next.Since = now
	next.AlertingSince = now
	next.Consecutive = 0
	next.Seeded = false
	if muted {
		// The state is true; the user has asked not to hear about it.
		// NotifyCount stays 0 so the eventual recovery is also silent, which
		// is what someone muting a node for maintenance wants.
		return Outcome{State: next, Changed: true, Entering: true}
	}
	next.NotifyCount++
	next.LastNotified = now
	return Outcome{State: next, Changed: true, Notify: true, Kind: KindAlert, Entering: true}
}

func recover_(next store.WatchState, now time.Time, muted bool) Outcome {
	wasAnnounced := next.NotifyCount > 0
	next.State = store.StateOK
	next.Since = now
	next.Consecutive = 0
	next.AlertingSince = time.Time{}
	next.NotifyCount = 0
	next.Seeded = false
	if muted || !wasAnnounced {
		// Nothing was ever announced for this episode — a seeded watch, or
		// a muted one — so announcing the recovery would be the first the
		// user heard of any of it.
		return Outcome{State: next, Changed: true}
	}
	next.LastNotified = now
	return Outcome{State: next, Changed: true, Notify: true, Kind: KindRecovered}
}

// ------------------------------------------------------------ breaker ----

// FeedVerdict is whether a poll's data should be believed and acted on.
type FeedVerdict struct {
	Status store.PollStatus
	Reason string
}

// FeedInputs is what JudgeFeed needs to decide.
type FeedInputs struct {
	FetchErr error
	// NodeCount is how many nodes the poll returned.
	NodeCount int
	// MaxRecentNodeCount is the largest recent healthy poll, the yardstick
	// for "implausibly small".
	MaxRecentNodeCount int
	// Advanced is how many targets' timestamps genuinely moved forward.
	Advanced int
	// ConsecutiveNonAdvancing counts prior polls, including this one, in
	// which nothing advanced.
	ConsecutiveNonAdvancing int

	MinNodes               int
	MinNodeFraction        float64
	MaxPollsWithoutAdvance int
	// HaveBaseline is false on a fresh install, where there is no history to
	// compare against and the advance check would fire spuriously.
	HaveBaseline bool
}

// JudgeFeed decides whether a poll may be evaluated.
//
// This is the guard against the failure that would destroy trust in the tool
// fastest: CoreScope having a bad day being read as "the entire mesh went
// offline" and fanning out to every user at once. Being wrong in the
// cautious direction costs a delayed alert; being wrong the other way costs
// every user's confidence at once, so the asymmetry is deliberate.
func JudgeFeed(in FeedInputs) FeedVerdict {
	if in.FetchErr != nil {
		return FeedVerdict{store.PollFailed, "upstream fetch failed: " + in.FetchErr.Error()}
	}
	if in.NodeCount < in.MinNodes {
		return FeedVerdict{store.PollSuspect,
			fmt.Sprintf("only %d nodes returned, below the floor of %d", in.NodeCount, in.MinNodes)}
	}
	if in.MaxRecentNodeCount > 0 {
		floor := int(float64(in.MaxRecentNodeCount) * in.MinNodeFraction)
		if in.NodeCount < floor {
			return FeedVerdict{store.PollSuspect,
				fmt.Sprintf("%d nodes is below %d, %.0f%% of the recent peak of %d",
					in.NodeCount, floor, in.MinNodeFraction*100, in.MaxRecentNodeCount)}
		}
	}
	// A full, well-formed response can still be frozen — CoreScope's own
	// upstream dying looks exactly like a healthy feed except that nothing
	// ever moves. A whole mesh going silent looks identical from here, and
	// mistaking a frozen feed for that would alert everyone at once.
	if in.HaveBaseline && in.Advanced == 0 && in.ConsecutiveNonAdvancing >= in.MaxPollsWithoutAdvance {
		return FeedVerdict{store.PollSuspect,
			fmt.Sprintf("no target has advanced across %d consecutive polls — the feed looks frozen",
				in.ConsecutiveNonAdvancing)}
	}
	return FeedVerdict{store.PollOK, ""}
}

// MinSignalsForFraction is how many watched signals must exist before the
// proportional limit means anything.
//
// Without this the fraction rule is actively harmful on a small deployment:
// with one watch, that one node failing is 100% of everything watched, so
// every genuine alert trips the breaker and nobody is ever told anything.
// That is precisely the period — the first users, the first trial — when
// silently swallowing alerts would be most damaging and least likely to be
// noticed. Below this floor the absolute limit governs on its own.
const MinSignalsForFraction = 20

// BreakerInputs is what TripBreaker needs.
type BreakerInputs struct {
	Entering      int
	ActiveSignals int
	MaxPerPoll    int
	MaxFraction   float64
}

// TripBreaker reports whether this poll's alerts should be withheld.
//
// The plausibility checks above catch the failures we thought of. This
// catches the ones we didn't: whatever the cause, several hundred watches
// entering alert simultaneously is far more likely to be our bug or their
// outage than every one of those nodes genuinely failing at the same instant.
// The operator hears about it; nobody else does, until it is understood.
func TripBreaker(in BreakerInputs) (tripped bool, reason string) {
	if in.Entering == 0 {
		return false, ""
	}
	if in.MaxPerPoll > 0 && in.Entering > in.MaxPerPoll {
		return true, fmt.Sprintf("%d watches would start alerting in one poll, over the limit of %d",
			in.Entering, in.MaxPerPoll)
	}
	if in.MaxFraction > 0 && in.ActiveSignals >= MinSignalsForFraction {
		frac := float64(in.Entering) / float64(in.ActiveSignals)
		if frac > in.MaxFraction {
			return true, fmt.Sprintf("%d of %d watched signals (%.0f%%) would start alerting in one poll, over the %.0f%% limit",
				in.Entering, in.ActiveSignals, frac*100, in.MaxFraction*100)
		}
	}
	return false, ""
}
