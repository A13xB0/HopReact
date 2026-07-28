package alert

import (
	"errors"
	"testing"
	"time"

	"hopreact/internal/store"
)

var t0 = time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)

const threshold = 6 * time.Hour

func pol(confirm int) Policy { return Policy{ConfirmPolls: confirm} }

// run drives a sequence of polls and returns the final state plus every
// notification that would have been sent. Asserting on the notification
// COUNT is the point of most of these tests — the failure mode that matters
// is not "wrong state" but "messaged the user five times".
type poll struct {
	after time.Duration // offset from t0 for this evaluation
	seen  time.Duration // how long ago the signal timestamp was, at that moment
	// absent marks a poll where the signal has no value at all.
	absent bool
	muted  bool
}

func run(t *testing.T, start store.WatchState, confirm int, polls []poll) (store.WatchState, []string) {
	t.Helper()
	cur := start
	var sent []string
	for i, p := range polls {
		now := t0.Add(p.after)
		obs := Observation{Applicable: true}
		if !p.absent {
			obs.At = now.Add(-p.seen)
		} else {
			obs.Applicable = false
		}
		out := Evaluate(now, obs, cur, threshold, p.muted, pol(confirm))
		cur = out.State
		if out.Notify {
			sent = append(sent, out.Kind)
		}
		_ = i
	}
	return cur, sent
}

func okState() store.WatchState {
	return store.WatchState{State: store.StateOK, Since: t0}
}

// The core promise: a target that goes down and stays down produces exactly
// one alert, then one recovery. Not one per poll.
func TestOneAlertThenOneRecovery(t *testing.T) {
	var polls []poll
	// 24 hours of polls, every 5 minutes, with the target silent throughout.
	for i := 0; i < 288; i++ {
		d := time.Duration(i) * 5 * time.Minute
		polls = append(polls, poll{after: d, seen: d + 7*time.Hour})
	}
	// Then it comes back and stays back.
	for i := 0; i < 10; i++ {
		d := 24*time.Hour + time.Duration(i)*5*time.Minute
		polls = append(polls, poll{after: d, seen: time.Minute})
	}

	final, sent := run(t, okState(), 2, polls)
	if len(sent) != 2 || sent[0] != KindAlert || sent[1] != KindRecovered {
		t.Fatalf("sent %v, want exactly [alert recovered]", sent)
	}
	if final.State != store.StateOK {
		t.Errorf("final state %q, want ok", final.State)
	}
}

// A target hovering either side of the threshold must not produce a stream
// of messages. This is the single most likely way to make the tool
// intolerable.
func TestFlappingProducesNoAlerts(t *testing.T) {
	var polls []poll
	for i := 0; i < 40; i++ {
		d := time.Duration(i) * 5 * time.Minute
		// Alternate just over and just under the 6h threshold.
		if i%2 == 0 {
			polls = append(polls, poll{after: d, seen: threshold + time.Minute})
		} else {
			polls = append(polls, poll{after: d, seen: threshold - time.Minute})
		}
	}
	_, sent := run(t, okState(), 2, polls)
	if len(sent) != 0 {
		t.Errorf("sent %v, want nothing — a crossing must survive ConfirmPolls", sent)
	}
}

// With ConfirmPolls=2 a single over-threshold reading must not alert; two in
// a row must.
func TestConfirmPollsGatesTheAlert(t *testing.T) {
	_, sent := run(t, okState(), 2, []poll{
		{after: 0, seen: threshold + time.Minute},
	})
	if len(sent) != 0 {
		t.Errorf("one poll over threshold sent %v, want nothing", sent)
	}

	_, sent = run(t, okState(), 2, []poll{
		{after: 0, seen: threshold + time.Minute},
		{after: 5 * time.Minute, seen: threshold + 6*time.Minute},
	})
	if len(sent) != 1 || sent[0] != KindAlert {
		t.Errorf("two consecutive polls over threshold sent %v, want one alert", sent)
	}
}

// A watch adopted onto an already-offline target must say nothing — and
// because nothing was announced, its eventual recovery must be silent too.
// On this network that's the common path: 526 of 779 targets have not been
// seen in over 24 hours.
func TestSeededWatchIsSilentBothWays(t *testing.T) {
	start := store.WatchState{State: store.StateUnknown, Since: t0}
	final, sent := run(t, start, 2, []poll{
		{after: 0, seen: 48 * time.Hour},               // adopt: already down
		{after: 5 * time.Minute, seen: 48 * time.Hour}, // still down
		{after: 10 * time.Minute, seen: time.Minute},   // back
		{after: 15 * time.Minute, seen: time.Minute},   // confirmed back
	})
	if len(sent) != 0 {
		t.Errorf("sent %v, want nothing at all for a watch that was never announced", sent)
	}
	if final.State != store.StateOK {
		t.Errorf("final state %q, want ok", final.State)
	}
}

// ...whereas a watch that WAS announced does get its recovery.
func TestAnnouncedAlertGetsARecovery(t *testing.T) {
	_, sent := run(t, okState(), 1, []poll{
		{after: 0, seen: threshold + time.Minute},
		{after: 5 * time.Minute, seen: time.Minute},
	})
	if len(sent) != 2 || sent[0] != KindAlert || sent[1] != KindRecovered {
		t.Errorf("sent %v, want [alert recovered]", sent)
	}
}

// Dropping back into alerting before a recovery is confirmed must not
// re-announce: the user was told it was down and, as far as they know, it
// never came back.
func TestRelapseBeforeConfirmedRecoveryIsSilent(t *testing.T) {
	_, sent := run(t, okState(), 2, []poll{
		{after: 0, seen: threshold + time.Minute},
		{after: 5 * time.Minute, seen: threshold + 6*time.Minute}, // alert
		{after: 10 * time.Minute, seen: time.Minute},              // recovering
		{after: 15 * time.Minute, seen: threshold + time.Hour},    // relapse
		{after: 20 * time.Minute, seen: threshold + 2*time.Hour},  // still down
	})
	if len(sent) != 1 || sent[0] != KindAlert {
		t.Errorf("sent %v, want a single alert", sent)
	}
}

// A signal with no value is unknown, not down. A target CoreScope has never
// mentioned, or a node that has never relayed, has not "stopped".
func TestAbsentSignalNeverAlerts(t *testing.T) {
	final, sent := run(t, store.WatchState{State: store.StateUnknown, Since: t0}, 1, []poll{
		{after: 0, absent: true},
		{after: 5 * time.Minute, absent: true},
		{after: 10 * time.Minute, absent: true},
	})
	if len(sent) != 0 {
		t.Errorf("sent %v, want nothing for a signal with no value", sent)
	}
	if final.State != store.StateUnknown {
		t.Errorf("state %q, want unknown", final.State)
	}
}

// A muted watch still tracks reality but says nothing, in either direction —
// which is what someone muting for planned maintenance wants.
func TestMutedWatchTracksButStaysSilent(t *testing.T) {
	final, sent := run(t, okState(), 1, []poll{
		{after: 0, seen: threshold + time.Minute, muted: true},
		{after: 5 * time.Minute, seen: time.Minute, muted: true},
	})
	if len(sent) != 0 {
		t.Errorf("sent %v, want silence while muted", sent)
	}
	if final.State != store.StateOK {
		t.Errorf("state %q, want the state to have tracked through to ok", final.State)
	}
}

// Entering is what the breaker counts, and it must be set exactly on the
// transition into alerting — not while already alerting.
func TestEnteringIsSetOnlyOnTheTransition(t *testing.T) {
	cur := okState()
	now := t0
	obs := Observation{At: now.Add(-threshold - time.Minute), Applicable: true}
	out := Evaluate(now, obs, cur, threshold, false, pol(1))
	if !out.Entering {
		t.Fatal("the transition into alerting must set Entering")
	}
	cur = out.State

	now = t0.Add(5 * time.Minute)
	obs = Observation{At: now.Add(-threshold - 6*time.Minute), Applicable: true}
	out = Evaluate(now, obs, cur, threshold, false, pol(1))
	if out.Entering {
		t.Error("staying in alerting must not count as entering")
	}
	if out.Notify {
		t.Error("staying in alerting must not notify")
	}
}

// ------------------------------------------------------- feed judging ----

func baseFeed() FeedInputs {
	return FeedInputs{
		NodeCount: 779, MaxRecentNodeCount: 779, Advanced: 12,
		MinNodes: 100, MinNodeFraction: 0.5, MaxPollsWithoutAdvance: 2,
		HaveBaseline: true,
	}
}

func TestJudgeFeedAcceptsAHealthyPoll(t *testing.T) {
	if v := JudgeFeed(baseFeed()); v.Status != store.PollOK {
		t.Errorf("got %q (%s), want ok", v.Status, v.Reason)
	}
}

// An upstream error must never be read as data. This is the single most
// important guard: "we fetched nothing" must not become "nothing is alive".
func TestJudgeFeedRejectsAFetchError(t *testing.T) {
	in := baseFeed()
	in.FetchErr = errors.New("connection refused")
	v := JudgeFeed(in)
	if v.Status != store.PollFailed {
		t.Errorf("got %q, want failed", v.Status)
	}
}

func TestJudgeFeedRejectsImplausiblySmallPolls(t *testing.T) {
	t.Run("below the absolute floor", func(t *testing.T) {
		in := baseFeed()
		in.NodeCount = 3
		if v := JudgeFeed(in); v.Status != store.PollSuspect {
			t.Errorf("got %q, want suspect", v.Status)
		}
	})
	t.Run("below the fraction of the recent peak", func(t *testing.T) {
		in := baseFeed()
		in.NodeCount = 300 // clears MinNodes but is well under half of 779
		if v := JudgeFeed(in); v.Status != store.PollSuspect {
			t.Errorf("got %q, want suspect", v.Status)
		}
	})
}

// A full, well-formed, entirely stationary feed is far more likely to be a
// dead upstream than an entire mesh falling silent between two polls.
func TestJudgeFeedRejectsAFrozenFeed(t *testing.T) {
	in := baseFeed()
	in.Advanced = 0
	in.ConsecutiveNonAdvancing = 2
	v := JudgeFeed(in)
	if v.Status != store.PollSuspect {
		t.Errorf("got %q, want suspect", v.Status)
	}

	// One quiet poll is not enough to call it frozen.
	in.ConsecutiveNonAdvancing = 1
	if v := JudgeFeed(in); v.Status != store.PollOK {
		t.Errorf("a single non-advancing poll gave %q, want ok", v.Status)
	}
}

// On a fresh install there is no history, so the advance check must not fire
// and block the very first poll.
func TestJudgeFeedSkipsTheAdvanceCheckWithoutABaseline(t *testing.T) {
	in := baseFeed()
	in.Advanced = 0
	in.ConsecutiveNonAdvancing = 5
	in.HaveBaseline = false
	in.MaxRecentNodeCount = 0
	if v := JudgeFeed(in); v.Status != store.PollOK {
		t.Errorf("got %q (%s), want ok on a first run", v.Status, v.Reason)
	}
}

// ----------------------------------------------------------- breaker ----

func TestBreakerAllowsNormalActivity(t *testing.T) {
	tripped, _ := TripBreaker(BreakerInputs{
		Entering: 2, ActiveSignals: 400, MaxPerPoll: 10, MaxFraction: 0.25,
	})
	if tripped {
		t.Error("a couple of nodes going down must not trip the breaker")
	}
}

func TestBreakerTripsOnAbsoluteCount(t *testing.T) {
	tripped, reason := TripBreaker(BreakerInputs{
		Entering: 50, ActiveSignals: 10000, MaxPerPoll: 10, MaxFraction: 0.25,
	})
	if !tripped {
		t.Fatal("50 simultaneous alerts should trip the absolute limit")
	}
	if reason == "" {
		t.Error("a tripped breaker must explain itself — it is the operator's only clue")
	}
}

// The fraction limit catches a deployment where the absolute limit is
// generous but the alert still covers most of what is being watched.
// Numbers must be at or above MinSignalsForFraction for the rule to apply
// at all — see TestBreakerDoesNotFireOnASmallDeployment.
func TestBreakerTripsOnFraction(t *testing.T) {
	tripped, _ := TripBreaker(BreakerInputs{
		Entering: 30, ActiveSignals: 60, MaxPerPoll: 1000, MaxFraction: 0.25,
	})
	if !tripped {
		t.Error("30 of 60 signals alerting at once should trip the fraction limit")
	}
}

func TestBreakerIgnoresAnEmptyPoll(t *testing.T) {
	if tripped, _ := TripBreaker(BreakerInputs{Entering: 0, ActiveSignals: 0, MaxPerPoll: 10, MaxFraction: 0.25}); tripped {
		t.Error("no transitions must never trip the breaker")
	}
}

// The fraction limit must not fire on a small deployment. With a single
// watch, that node failing is 100% of everything watched — if the
// proportional rule applied there, every genuine alert would be swallowed
// during exactly the period (first users, first trial) when that would be
// most damaging and least likely to be spotted.
func TestBreakerDoesNotFireOnASmallDeployment(t *testing.T) {
	for _, n := range []int{1, 2, 5, 19} {
		tripped, reason := TripBreaker(BreakerInputs{
			Entering: n, ActiveSignals: n, MaxPerPoll: 100, MaxFraction: 0.25,
		})
		if tripped {
			t.Errorf("%d of %d signals tripped the breaker (%s) — the fraction rule needs a floor", n, n, reason)
		}
	}
	// At the floor it starts applying again.
	if tripped, _ := TripBreaker(BreakerInputs{
		Entering: 20, ActiveSignals: 20, MaxPerPoll: 100, MaxFraction: 0.25,
	}); !tripped {
		t.Error("once there are enough signals, the fraction rule should apply")
	}
}

// The absolute limit still protects a small deployment.
func TestBreakerAbsoluteLimitStillAppliesWhenSmall(t *testing.T) {
	if tripped, _ := TripBreaker(BreakerInputs{
		Entering: 11, ActiveSignals: 12, MaxPerPoll: 10, MaxFraction: 0.25,
	}); !tripped {
		t.Error("the absolute limit must still apply below the fraction floor")
	}
}
