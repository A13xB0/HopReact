package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
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

	if got := mustState(t, s, id, SourceRelayed).State; got != StateUnknown {
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

	st := mustState(t, s, id, SourceSeen)
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
	if st := mustState(t, s, id, SourceSeen); st.State != StateOK {
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
	if st := mustState(t, s, id, SourceSeen); st.State != StateUnknown {
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
	var haveRelay bool
	for _, rv := range views[0].Rules {
		if rv.Rule.Source == SourceRelayed {
			haveRelay = true
		}
	}
	if !haveRelay {
		t.Error("a watch with AlertOnRelay should carry a relay rule")
	}
	if views[1].Target != nil {
		t.Error("an unknown target must come back nil, not zero-valued")
	}
}

// ------------------------------------------------------------- helpers --

// mustState returns the alert state of the watch's rule with the given
// source. State is keyed by rule now, and a watch's rules are created for it,
// so tests reach it through the source they mean rather than a rule id they
// would have to look up anyway.
func mustState(t *testing.T, s *Store, watchID int64, src Source) WatchState {
	t.Helper()
	ctx := context.Background()
	rules, err := s.RulesForWatch(ctx, watchID)
	if err != nil {
		t.Fatal(err)
	}
	states, err := s.AllWatchState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rules {
		if r.Source != src {
			continue
		}
		st, ok := states[watchID][r.ID]
		if !ok {
			t.Fatalf("rule %d (%s) on watch %d has no state row", r.ID, src, watchID)
		}
		return st
	}
	t.Fatalf("watch %d has no %s rule", watchID, src)
	return WatchState{}
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

// ------------------------------------------------------------ migration --

// openV1 builds a database at the pre-rules schema and returns its directory.
func openV1(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "hopreact.db")+
		"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	body, err := migrationFS.ReadFile("migrations/0001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(body)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 1`); err != nil {
		t.Fatal(err)
	}
	return dir
}

// Upgrading must not change what anyone is alerted on. Every existing watch
// keeps CoreScope's own last_seen as its signal — NOT an equivalent built
// from per-type evidence, which sees only about 41% of packets and would
// quietly tighten every threshold in the system.
func TestMigrationPreservesExistingAlerting(t *testing.T) {
	dir := openV1(t)
	ts := base.Add(-30 * time.Hour).Unix()

	func() {
		db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "hopreact.db")+"?_pragma=foreign_keys(ON)&_txlock=immediate")
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err := db.Exec(`
			INSERT INTO users (id, discord_id, username, created_at, last_login_at)
				VALUES (1,'d1','alice',0,0);
			INSERT INTO targets (kind, key, name, role, last_seen_at, last_relayed_at,
				relay_ever_observed_at, first_indexed_at, last_in_feed_at, updated_at)
				VALUES ('node','aa','n-aa','repeater',` + fmt.Sprint(ts) + `,` + fmt.Sprint(ts) + `,` + fmt.Sprint(ts) + `,0,0,0);
			-- one plain watch, one that opted into the relay signal
			INSERT INTO watches (id, user_id, target_kind, target_key, threshold_hours, alert_on_relay, created_at)
				VALUES (1,1,'node','aa',6,0,0), (2,1,'node','bb',12,1,0);
			-- watch 1 is mid-outage and has ALREADY been announced once
			INSERT INTO watch_state (watch_id, signal, state, since, consecutive, observed_at, notify_count, seeded)
				VALUES (1,'seen','alerting',0,0,` + fmt.Sprint(ts) + `,1,0);
			INSERT INTO watch_state (watch_id, signal, state, since, consecutive, observed_at, notify_count, seeded)
				VALUES (2,'seen','ok',0,0,` + fmt.Sprint(ts) + `,0,0),
				       (2,'relayed','alerting',0,0,` + fmt.Sprint(ts) + `,1,0);
		`); err != nil {
			t.Fatal(err)
		}
	}()

	now := base
	s, err := Open(dir, func() time.Time { return now })
	if err != nil {
		t.Fatalf("migrating a v1 database: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()

	// Watch 1: exactly the one signal it had, still reading last_seen.
	r1, err := s.RulesForWatch(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(r1) != 1 {
		t.Fatalf("watch 1 has %d rules, want 1 — nothing new may be added to an existing watch", len(r1))
	}
	if r1[0].Source != SourceSeen {
		t.Errorf("rule source = %q, want %q; rebuilding it from per-type evidence would tighten it silently",
			r1[0].Source, SourceSeen)
	}
	if r1[0].ThresholdHours != 6 {
		t.Errorf("threshold = %d, want the watch's original 6", r1[0].ThresholdHours)
	}

	// The alert already announced must stay announced. Losing notify_count
	// would let a watch that is already alerting announce itself a second
	// time the moment the upgrade lands.
	st := mustState(t, s, 1, SourceSeen)
	if st.State != StateAlerting {
		t.Errorf("state = %q, want it carried across the migration", st.State)
	}
	if st.NotifyCount != 1 {
		t.Errorf("NotifyCount = %d, want 1 — otherwise the outage is announced twice", st.NotifyCount)
	}

	// Watch 2 opted into the relay signal, so it gets both rules and no more.
	r2, _ := s.RulesForWatch(ctx, 2)
	if len(r2) != 2 {
		t.Fatalf("watch 2 has %d rules, want 2", len(r2))
	}
	sources := map[Source]bool{}
	for _, r := range r2 {
		sources[r.Source] = true
		if r.ThresholdHours != 12 {
			t.Errorf("rule %q threshold = %d, want 12", r.Source, r.ThresholdHours)
		}
	}
	if !sources[SourceSeen] || !sources[SourceRelayed] {
		t.Errorf("watch 2 rules = %v, want both seen and relayed", sources)
	}
	if got := mustState(t, s, 2, SourceRelayed).State; got != StateAlerting {
		t.Errorf("relay state = %q, want it carried across", got)
	}
	if got := mustState(t, s, 2, SourceSeen).State; got != StateOK {
		t.Errorf("seen state = %q, want it carried across", got)
	}
}

// A fresh install and an upgraded one must end up with the same schema, or
// the two diverge silently and only one of them is ever tested.
func TestMigratedSchemaMatchesFresh(t *testing.T) {
	upgraded, err := Open(openV1(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	fresh, err := Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()

	read := func(s *Store) map[string]string {
		rows, err := s.read.Query(
			`SELECT name, COALESCE(sql,'') FROM sqlite_master WHERE type='table' ORDER BY name`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]string{}
		for rows.Next() {
			var name, ddl string
			if err := rows.Scan(&name, &ddl); err != nil {
				t.Fatal(err)
			}
			out[name] = ddl
		}
		return out
	}

	a, b := read(upgraded), read(fresh)
	for name, ddl := range b {
		got, ok := a[name]
		if !ok {
			t.Errorf("upgraded database is missing table %q", name)
			continue
		}
		if got != ddl {
			t.Errorf("table %q differs after upgrade:\n upgraded: %s\n fresh:    %s", name, got, ddl)
		}
	}
	for name := range a {
		if _, ok := b[name]; !ok {
			t.Errorf("upgraded database has a leftover table %q", name)
		}
	}
}

// ------------------------------------------------------------- retention --

// Almost everything here is bounded by the size of the mesh: one row per
// target, and one per target per payload type per direction. No packet is
// ever stored — only the timestamp of the most recent one of each kind. These
// three tables are the exception, growing with time instead, and a poll every
// five minutes is about 105,000 poll_runs a year on its own.
func TestPruneDropsOnlyOldHistory(t *testing.T) {
	s, now := testStore(t)
	ctx := context.Background()
	u, _ := s.UpsertUser(ctx, "d1", "alice", "")

	old := base.Add(-400 * 24 * time.Hour).Unix()
	recent := base.Add(-time.Hour).Unix()

	err := s.tx(ctx, func(tx *sql.Tx) error {
		for _, at := range []int64{old, recent} {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO poll_runs (started_at, status) VALUES (?, 'ok')`, at); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO alert_events (watch_id, rule_id, signal, from_state, to_state, at, notified)
				 VALUES (1, 1, 'x', 'ok', 'alerting', ?, 1)`, at); err != nil {
				return err
			}
			// One delivered and one still queued at each age.
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO notifications (user_id, kind, payload, created_at, send_after, sent_at)
				 VALUES (?, 'alert', '{}', ?, ?, ?)`, u.ID, at, at, at); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO notifications (user_id, kind, payload, created_at, send_after, sent_at)
				 VALUES (?, 'alert', '{}', ?, ?, NULL)`, u.ID, at, at); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	*now = base
	if _, err := s.Prune(ctx); err != nil {
		t.Fatal(err)
	}

	if n := countRows(t, s, "poll_runs"); n != 1 {
		t.Errorf("poll_runs = %d, want 1 (the recent one)", n)
	}
	if n := countRows(t, s, "alert_events"); n != 1 {
		t.Errorf("alert_events = %d, want 1", n)
	}
	// An undelivered message is never pruned by age — dropping one silently
	// would lose an alert somebody is still owed.
	var unsent int
	if err := s.read.QueryRow(`SELECT COUNT(*) FROM notifications WHERE sent_at IS NULL`).Scan(&unsent); err != nil {
		t.Fatal(err)
	}
	if unsent != 2 {
		t.Errorf("unsent notifications = %d, want both kept regardless of age", unsent)
	}
	var sent int
	if err := s.read.QueryRow(`SELECT COUNT(*) FROM notifications WHERE sent_at IS NOT NULL`).Scan(&sent); err != nil {
		t.Fatal(err)
	}
	if sent != 1 {
		t.Errorf("delivered notifications = %d, want only the recent one", sent)
	}
}

// The activity table must stay one row per (target, type, direction) no
// matter how much traffic flows through — it records the latest time each
// kind of packet was seen, not the packets themselves.
func TestActivityDoesNotGrowWithTraffic(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	for i := 0; i < 500; i++ {
		err := s.tx(ctx, func(tx *sql.Tx) error {
			return s.UpsertActivity(ctx, tx, "node", "aa", 4, DirCarried,
				base.Add(time.Duration(i)*time.Minute), 1)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := countRows(t, s, "target_activity"); n != 1 {
		t.Fatalf("500 packets produced %d rows, want 1", n)
	}
	acts, _ := s.ActivityFor(ctx, "node", "aa")
	if len(acts) != 1 || acts[0].EvidenceCount != 500 {
		t.Fatalf("got %+v, want one row counting 500", acts)
	}
	if !acts[0].LastAt.Equal(base.Add(499 * time.Minute)) {
		t.Errorf("LastAt = %v, want the newest", acts[0].LastAt)
	}
}

// A new watch starts with the SAME ready-made rules its settings page
// offers — the chosen template's tuned rule plus the backstop — so the two
// lists can never drift apart again.
func TestCreateWatchSeedsChosenTemplate(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	if _, err := s.UpsertTargets(ctx, []corescope.Observation{node("aa", base.Add(-time.Minute), time.Time{})}); err != nil {
		t.Fatal(err)
	}
	u, _ := s.UpsertUser(ctx, "d1", "alice", "")

	// No explicit hours: the template's tuned threshold governs.
	id, err := s.CreateWatch(ctx, Watch{
		UserID: u.ID, TargetKind: "node", TargetKey: "aa", TemplateID: "adverts",
	}, 50)
	if err != nil {
		t.Fatal(err)
	}
	rules, err := s.RulesForWatch(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 2 {
		t.Fatalf("got %d rules, want the template rule and the backstop", len(rules))
	}
	if rules[0].Label != "Adverts only" || rules[0].ThresholdHours != 25 {
		t.Errorf("first rule = %q at %dh, want the Adverts only template at its tuned 25h",
			rules[0].Label, rules[0].ThresholdHours)
	}
	if rules[1].Label != "Not heard at all" || rules[1].ThresholdHours != 25 {
		t.Errorf("backstop = %q at %dh, want it at the template's threshold",
			rules[1].Label, rules[1].ThresholdHours)
	}
}

// A template for the wrong kind of target must not seed rules that can never
// fire; the kind's own default governs instead.
func TestCreateWatchWrongKindTemplateFallsBack(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	u, _ := s.UpsertUser(ctx, "d1", "alice", "")

	id, err := s.CreateWatch(ctx, Watch{
		UserID: u.ID, TargetKind: "observer", TargetKey: "obs1", TemplateID: "adverts",
	}, 50)
	if err != nil {
		t.Fatal(err)
	}
	rules, err := s.RulesForWatch(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 || rules[0].Label != "Standard observer" || rules[0].Source != SourceSeen {
		t.Errorf("rules = %+v, want only the Standard observer template", rules)
	}
}
