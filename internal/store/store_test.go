package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"hopreact/internal/corescope"
)

var base = time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)

// testStore returns a Store on a temp file with a clock the test drives.
func testStore(t *testing.T) (*Store, *time.Time) {
	t.Helper()
	now := base
	s, err := Open(t.TempDir(), func() time.Time { return now })
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, &now
}

func TestOpenIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	s1, err := Open(dir, nil)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	s1.Close()

	// Re-opening must apply no migrations and lose nothing.
	s2, err := Open(dir, nil)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer s2.Close()
	if _, err := s2.CountTargets(context.Background()); err != nil {
		t.Fatalf("schema unusable after reopen: %v", err)
	}
}

func node(key string, seen, relayed time.Time) corescope.Observation {
	return corescope.Observation{
		Kind: corescope.KindNode, Key: key, Name: "n-" + key,
		Role: "repeater", LastSeen: seen, LastRelayed: relayed,
	}
}

// A target's timestamps must only ever move forward. CoreScope can serve a
// stale or partial view, and a regression would manufacture a false alert
// out of nothing.
func TestUpsertTargetsIsMonotonic(t *testing.T) {
	s, now := testStore(t)
	ctx := context.Background()

	fresh := base.Add(-time.Minute)
	if _, err := s.UpsertTargets(ctx, []corescope.Observation{node("aa", fresh, fresh)}); err != nil {
		t.Fatal(err)
	}

	// A later poll reporting an OLDER last_seen — the regression case.
	stale := base.Add(-10 * time.Hour)
	advanced, err := s.UpsertTargets(ctx, []corescope.Observation{node("aa", stale, stale)})
	if err != nil {
		t.Fatal(err)
	}
	if advanced != 0 {
		t.Errorf("advanced = %d, want 0 for a regressing poll", advanced)
	}

	got, err := s.Target(ctx, "node", "aa")
	if err != nil {
		t.Fatal(err)
	}
	if !got.LastSeen.Equal(fresh) {
		t.Errorf("LastSeen = %v, want it held at the newer %v", got.LastSeen, fresh)
	}
	if !got.LastRelayed.Equal(fresh) {
		t.Errorf("LastRelayed = %v, want it held at %v", got.LastRelayed, fresh)
	}

	// A genuine advance counts, which is what the frozen-feed check reads.
	*now = base.Add(time.Minute)
	advanced, err = s.UpsertTargets(ctx, []corescope.Observation{node("aa", base, base)})
	if err != nil {
		t.Fatal(err)
	}
	if advanced != 1 {
		t.Errorf("advanced = %d, want 1", advanced)
	}
}

// Once a target has relayed it has relayed. A later feed omitting
// last_relayed must not make an established relay look like one that never
// started, or the relay alert would be permanently disarmed for it.
func TestRelayEverObservedIsSticky(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()

	if _, err := s.UpsertTargets(ctx, []corescope.Observation{node("aa", base, base)}); err != nil {
		t.Fatal(err)
	}
	// Now a poll with no relay information at all.
	if _, err := s.UpsertTargets(ctx, []corescope.Observation{node("aa", base, time.Time{})}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Target(ctx, "node", "aa")
	if got.RelayEverObserved.IsZero() {
		t.Error("RelayEverObserved was cleared — the relay signal would be wrongly disarmed")
	}
	if !got.LastRelayed.Equal(base) {
		t.Errorf("LastRelayed = %v, want it retained", got.LastRelayed)
	}
}

// 189 nodes on the live instance have never relayed. Their relay signal must
// stay unknown rather than alerting for a stoppage that never happened.
func TestNeverRelayedTargetKeepsRelayUnknown(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	if _, err := s.UpsertTargets(ctx, []corescope.Observation{node("aa", base, time.Time{})}); err != nil {
		t.Fatal(err)
	}
	u, _ := s.UpsertUser(ctx, "d1", "alice", "")

	id, err := s.CreateWatch(ctx, Watch{
		UserID: u.ID, TargetKind: "node", TargetKey: "aa",
		ThresholdHours: 1, AlertOnRelay: true,
	}, 50)
	if err != nil {
		t.Fatal(err)
	}

	states, _ := s.AllWatchState(ctx)
	if got := states[id][SignalRelayed].State; got != StateUnknown {
		t.Errorf("relay state = %q, want %q for a target that has never relayed", got, StateUnknown)
	}
}

// A watch created against an already-offline target starts alerting but
// notifies nobody: an alert for something broken before you asked to watch
// it is noise. notify_count staying 0 also suppresses the later recovery, so
// we never announce recovery from an alert nobody was told about.
func TestCreateWatchSeedsAlreadyOfflineTargetSilently(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	long := base.Add(-48 * time.Hour)
	if _, err := s.UpsertTargets(ctx, []corescope.Observation{node("aa", long, long)}); err != nil {
		t.Fatal(err)
	}
	u, _ := s.UpsertUser(ctx, "d1", "alice", "")

	id, err := s.CreateWatch(ctx, Watch{
		UserID: u.ID, TargetKind: "node", TargetKey: "aa", ThresholdHours: 6,
	}, 50)
	if err != nil {
		t.Fatal(err)
	}

	st := mustState(t, s, id, SignalSeen)
	if st.State != StateAlerting {
		t.Errorf("state = %q, want %q", st.State, StateAlerting)
	}
	if st.NotifyCount != 0 {
		t.Errorf("NotifyCount = %d, want 0 so nothing is sent", st.NotifyCount)
	}
	if !st.Seeded {
		t.Error("Seeded should be set so the dashboard can explain the silence")
	}
	if n := countRows(t, s, "notifications"); n != 0 {
		t.Errorf("%d notifications queued, want 0", n)
	}
}

// The same watch on a healthy target starts ok, not alerting.
func TestCreateWatchOnHealthyTargetStartsOK(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	if _, err := s.UpsertTargets(ctx, []corescope.Observation{node("aa", base.Add(-time.Minute), base)}); err != nil {
		t.Fatal(err)
	}
	u, _ := s.UpsertUser(ctx, "d1", "alice", "")
	id, err := s.CreateWatch(ctx, Watch{UserID: u.ID, TargetKind: "node", TargetKey: "aa", ThresholdHours: 6}, 50)
	if err != nil {
		t.Fatal(err)
	}
	if st := mustState(t, s, id, SignalSeen); st.State != StateOK {
		t.Errorf("state = %q, want %q", st.State, StateOK)
	}
}

// A watch on a key CoreScope has never reported stays unknown and never
// alerts — absence of a target is not evidence it is down.
func TestWatchOnUnknownTargetStaysUnknown(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	u, _ := s.UpsertUser(ctx, "d1", "alice", "")
	id, err := s.CreateWatch(ctx, Watch{UserID: u.ID, TargetKind: "node", TargetKey: "ghost", ThresholdHours: 1}, 50)
	if err != nil {
		t.Fatal(err)
	}
	if st := mustState(t, s, id, SignalSeen); st.State != StateUnknown {
		t.Errorf("state = %q, want %q", st.State, StateUnknown)
	}
}

func TestWatchLimitAndDuplicates(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	u, _ := s.UpsertUser(ctx, "d1", "alice", "")

	w := Watch{UserID: u.ID, TargetKind: "node", TargetKey: "aa", ThresholdHours: 1}
	if _, err := s.CreateWatch(ctx, w, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateWatch(ctx, w, 2); !errors.Is(err, ErrDuplicateWatch) {
		t.Errorf("second identical watch: got %v, want ErrDuplicateWatch", err)
	}

	w2 := w
	w2.TargetKey = "bb"
	if _, err := s.CreateWatch(ctx, w2, 2); err != nil {
		t.Fatal(err)
	}
	w3 := w
	w3.TargetKey = "cc"
	if _, err := s.CreateWatch(ctx, w3, 2); !errors.Is(err, ErrWatchLimit) {
		t.Errorf("over the cap: got %v, want ErrWatchLimit", err)
	}
}

// Non-exclusive watching is the whole claiming model: two people watching
// one node with different thresholds must both work.
func TestTwoUsersMayWatchTheSameTarget(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	a, _ := s.UpsertUser(ctx, "d1", "alice", "")
	b, _ := s.UpsertUser(ctx, "d2", "bob", "")

	if _, err := s.CreateWatch(ctx, Watch{UserID: a.ID, TargetKind: "node", TargetKey: "aa", ThresholdHours: 1}, 50); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateWatch(ctx, Watch{UserID: b.ID, TargetKind: "node", TargetKey: "aa", ThresholdHours: 24}, 50); err != nil {
		t.Fatalf("a second user must be able to watch the same target: %v", err)
	}
	if n, _ := s.CountWatches(ctx); n != 2 {
		t.Errorf("watches = %d, want 2", n)
	}
}

// ON DELETE CASCADE only works if foreign_keys is actually ON, which is
// per-connection and defaults OFF — so this is really a test that the DSN
// pragma is present. Without it, deleting an account silently orphans every
// row it owned, forever.
func TestDeleteUserCascades(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	u, _ := s.UpsertUser(ctx, "d1", "alice", "")
	if _, err := s.UpsertTargets(ctx, []corescope.Observation{node("aa", base, base)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateWatch(ctx, Watch{UserID: u.ID, TargetKind: "node", TargetKey: "aa", ThresholdHours: 1}, 50); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession(ctx, []byte("hash"), u.ID, "csrf", base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteTx(ctx, func(tx *dbTx) error {
		return s.QueueNotification(ctx, tx, u.ID, "alert", "{}", base)
	}); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteUser(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"watches", "sessions", "notifications", "watch_state"} {
		if n := countRows(t, s, table); n != 0 {
			t.Errorf("%s still has %d rows after the owning user was deleted", table, n)
		}
	}
}

func TestSessionLifecycle(t *testing.T) {
	s, now := testStore(t)
	ctx := context.Background()
	u, _ := s.UpsertUser(ctx, "d1", "alice", "")
	hash := []byte("token-hash")

	if err := s.CreateSession(ctx, hash, u.ID, "csrf-token", base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, csrf, err := s.SessionUser(ctx, hash)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != u.ID || csrf != "csrf-token" {
		t.Errorf("got user %d csrf %q", got.ID, csrf)
	}

	// Expiry is enforced on read, not left to the cleanup job.
	*now = base.Add(2 * time.Hour)
	if _, _, err := s.SessionUser(ctx, hash); !errors.Is(err, ErrNotFound) {
		t.Errorf("expired session: got %v, want ErrNotFound", err)
	}
	n, err := s.DeleteExpiredSessions(ctx)
	if err != nil || n != 1 {
		t.Errorf("DeleteExpiredSessions = %d, %v; want 1, nil", n, err)
	}
}

func TestSearchTargets(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	obs := []corescope.Observation{
		node("aaa", base.Add(-time.Hour), base),
		node("bbb", base, base),
	}
	obs[0].Name = "Ben Nevis Repeater"
	obs[1].Name = "Cairngorm"
	if _, err := s.UpsertTargets(ctx, obs); err != nil {
		t.Fatal(err)
	}

	found, err := s.SearchTargets(ctx, "nevis", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].Key != "aaa" {
		t.Errorf("search by name returned %+v", found)
	}
	if found, _ = s.SearchTargets(ctx, "bbb", 10); len(found) != 1 {
		t.Errorf("search by key returned %d results, want 1", len(found))
	}
	// The empty query lists most-recently-seen first: a user is far more
	// likely to want a live node than a long-dead one.
	all, _ := s.SearchTargets(ctx, "", 10)
	if len(all) != 2 || all[0].Key != "bbb" {
		t.Errorf("empty query should list freshest first, got %+v", all)
	}
}

func TestPollRunAccounting(t *testing.T) {
	s, now := testStore(t)
	ctx := context.Background()

	id, err := s.StartPollRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// A run that is started but never finished must not read as healthy —
	// it defaults to 'failed' precisely so a crash mid-poll can't leave
	// behind a row that looks like a good poll.
	runs, _ := s.RecentPollRuns(ctx, 5)
	if len(runs) != 1 || runs[0].Status != PollFailed {
		t.Fatalf("an unfinished run should default to failed, got %+v", runs)
	}

	if err := s.FinishPollRun(ctx, PollRun{ID: id, Status: PollOK, NodeCount: 779, ObserverCount: 9, AdvancedCount: 12, Evaluated: true}); err != nil {
		t.Fatal(err)
	}
	if max, _ := s.MaxRecentNodeCount(ctx, base.Add(-time.Hour)); max != 779 {
		t.Errorf("MaxRecentNodeCount = %d, want 779", max)
	}

	// Two consecutive polls where nothing advanced is what the frozen-feed
	// check counts.
	for i := 0; i < 2; i++ {
		*now = now.Add(5 * time.Minute)
		rid, _ := s.StartPollRun(ctx)
		if err := s.FinishPollRun(ctx, PollRun{ID: rid, Status: PollOK, NodeCount: 779, AdvancedCount: 0}); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.ConsecutiveNonAdvancingPolls(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("ConsecutiveNonAdvancingPolls = %d, want 2", n)
	}
}

func TestNotificationOutbox(t *testing.T) {
	s, now := testStore(t)
	ctx := context.Background()
	u, _ := s.UpsertUser(ctx, "d1", "alice", "")

	if err := s.WriteTx(ctx, func(tx *dbTx) error {
		return s.QueueNotification(ctx, tx, u.ID, "alert", `{"x":1}`, time.Time{})
	}); err != nil {
		t.Fatal(err)
	}

	pending, err := s.PendingNotifications(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].DiscordID != "d1" {
		t.Fatalf("pending = %+v", pending)
	}

	// A failure backs off, so a Discord outage doesn't spin.
	if err := s.MarkNotificationFailed(ctx, pending[0].ID, 0, "boom"); err != nil {
		t.Fatal(err)
	}
	if again, _ := s.PendingNotifications(ctx, 10); len(again) != 0 {
		t.Error("a failed notification should not be immediately due again")
	}
	*now = now.Add(10 * time.Minute)
	if again, _ := s.PendingNotifications(ctx, 10); len(again) != 1 {
		t.Error("it should become due after the backoff")
	}

	if err := s.MarkNotificationSent(ctx, pending[0].ID); err != nil {
		t.Fatal(err)
	}
	if again, _ := s.PendingNotifications(ctx, 10); len(again) != 0 {
		t.Error("a sent notification must not be returned again")
	}
}

func TestSetDMStatus(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	u, _ := s.UpsertUser(ctx, "d1", "alice", "")
	if !u.DMOK {
		t.Error("a new user should start deliverable")
	}
	if err := s.SetDMStatus(ctx, u.ID, false, "left the server"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.UserByID(ctx, u.ID)
	if got.DMOK || got.DMFailedReason != "left the server" {
		t.Errorf("DMOK=%v reason=%q", got.DMOK, got.DMFailedReason)
	}
}

func TestWatchViewsJoinsTargetAndState(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	if _, err := s.UpsertTargets(ctx, []corescope.Observation{node("aa", base, base)}); err != nil {
		t.Fatal(err)
	}
	u, _ := s.UpsertUser(ctx, "d1", "alice", "")
	if _, err := s.CreateWatch(ctx, Watch{UserID: u.ID, TargetKind: "node", TargetKey: "aa", ThresholdHours: 6, AlertOnRelay: true}, 50); err != nil {
		t.Fatal(err)
	}
	// ...and one on a target CoreScope has never mentioned.
	if _, err := s.CreateWatch(ctx, Watch{UserID: u.ID, TargetKind: "node", TargetKey: "ghost", ThresholdHours: 6}, 50); err != nil {
		t.Fatal(err)
	}

	views, err := s.WatchViews(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 2 {
		t.Fatalf("got %d views, want 2", len(views))
	}
	if views[0].Target == nil || views[0].Target.Name != "n-aa" {
		t.Error("the known target should be joined")
	}
	if views[0].Relay == nil {
		t.Error("a watch with AlertOnRelay should carry relay state")
	}
	if views[1].Target != nil {
		t.Error("an unknown target must come back nil, not zero-valued")
	}
}

// ------------------------------------------------------------- helpers --

func mustState(t *testing.T, s *Store, watchID int64, sig Signal) WatchState {
	t.Helper()
	states, err := s.AllWatchState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	st, ok := states[watchID][sig]
	if !ok {
		t.Fatalf("no %s state for watch %d", sig, watchID)
	}
	return st
}

func countRows(t *testing.T, s *Store, table string) int {
	t.Helper()
	var n int
	// table is a literal from the test, never input.
	if err := s.read.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
