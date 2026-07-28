package poller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"hopreact/internal/config"
	"hopreact/internal/corescope"
	"hopreact/internal/store"
)

var t0 = time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)

// fakeScope serves whatever the test currently wants CoreScope to say.
type fakeScope struct {
	nodes  []map[string]any
	status int
	srv    *httptest.Server
}

func newFakeScope(t *testing.T) *fakeScope {
	t.Helper()
	f := &fakeScope{status: http.StatusOK}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/nodes", func(w http.ResponseWriter, r *http.Request) {
		if f.status != http.StatusOK {
			http.Error(w, "upstream sad", f.status)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"nodes": f.nodes, "total": len(f.nodes)})
	})
	mux.HandleFunc("/api/observers", func(w http.ResponseWriter, r *http.Request) {
		if f.status != http.StatusOK {
			http.Error(w, "upstream sad", f.status)
			return
		}
		fmt.Fprint(w, `{"observers":[]}`)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// setNodes replaces the feed with n nodes, all last seen `age` ago from now.
func (f *fakeScope) setNodes(now time.Time, n int, age time.Duration) {
	f.nodes = nil
	for i := 0; i < n; i++ {
		f.nodes = append(f.nodes, map[string]any{
			"public_key":   fmt.Sprintf("%064x", i),
			"name":         fmt.Sprintf("node-%d", i),
			"role":         "repeater",
			"last_seen":    now.Add(-age).Format(time.RFC3339),
			"last_relayed": now.Add(-age).Format(time.RFC3339),
		})
	}
}

type harness struct {
	st    *store.Store
	scope *fakeScope
	pl    *Poller
	now   time.Time
}

func newHarness(t *testing.T, tune func(*config.Config)) *harness {
	t.Helper()
	h := &harness{now: t0}
	st, err := store.Open(t.TempDir(), func() time.Time { return h.now })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	cfg := config.Default()
	cfg.Alerts.ConfirmPolls = 1 // most tests care about policy, not confirmation
	if tune != nil {
		tune(&cfg)
	}
	scope := newFakeScope(t)
	h.st, h.scope = st, scope
	h.pl = &Poller{
		Store: st,
		Scope: corescope.NewClient(scope.srv.URL, 5*time.Second, scope.srv.Client()),
		Cfg:   cfg,
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:   func() time.Time { return h.now },
	}
	return h
}

func (h *harness) poll(t *testing.T) {
	t.Helper()
	if err := h.pl.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
}

func (h *harness) queued(t *testing.T) []store.Notification {
	t.Helper()
	n, err := h.st.PendingNotifications(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func (h *harness) watch(t *testing.T, key string, hours int) int64 {
	t.Helper()
	u, err := h.st.UpsertUser(context.Background(), "d1", "alice", "")
	if err != nil {
		t.Fatal(err)
	}
	id, err := h.st.CreateWatch(context.Background(), store.Watch{
		UserID: u.ID, TargetKind: "node", TargetKey: key, ThresholdHours: hours,
	}, 50)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func key(i int) string { return fmt.Sprintf("%064x", i) }

// The happy path end to end: a healthy feed, a node goes quiet, exactly one
// notification is queued, and it comes back with exactly one more.
func TestPollAlertsOnceAndRecoversOnce(t *testing.T) {
	h := newHarness(t, nil)

	h.scope.setNodes(h.now, 200, time.Minute)
	h.poll(t)
	h.watch(t, key(0), 6)

	// Node 0 goes quiet; everything else stays fresh.
	h.now = t0.Add(7 * time.Hour)
	h.scope.setNodes(h.now, 200, time.Minute)
	h.scope.nodes[0]["last_seen"] = h.now.Add(-7 * time.Hour).Format(time.RFC3339)
	h.poll(t)

	q := h.queued(t)
	if len(q) != 1 || q[0].Kind != "alert" {
		t.Fatalf("queued %d messages %v, want one alert", len(q), q)
	}

	// Still down several polls later — no further messages.
	for i := 1; i <= 5; i++ {
		h.now = t0.Add(7*time.Hour + time.Duration(i)*5*time.Minute)
		h.scope.setNodes(h.now, 200, time.Minute)
		h.scope.nodes[0]["last_seen"] = t0.Format(time.RFC3339)
		h.poll(t)
	}
	if q := h.queued(t); len(q) != 1 {
		t.Fatalf("queued %d, want still 1 — a persistent outage must not repeat", len(q))
	}

	// Back.
	h.now = t0.Add(9 * time.Hour)
	h.scope.setNodes(h.now, 200, time.Minute)
	h.poll(t)
	q = h.queued(t)
	if len(q) != 2 || q[1].Kind != "recovered" {
		t.Fatalf("queued %v, want an added recovery", q)
	}
}

// The most important test in the project: an upstream failure must not be
// read as "every node went offline".
func TestUpstreamFailureNotifiesNobody(t *testing.T) {
	h := newHarness(t, nil)
	h.scope.setNodes(h.now, 200, time.Minute)
	h.poll(t)
	h.watch(t, key(0), 6)

	// CoreScope falls over.
	h.now = t0.Add(10 * time.Hour)
	h.scope.status = http.StatusInternalServerError
	h.poll(t)

	if q := h.queued(t); len(q) != 0 {
		t.Fatalf("queued %d messages during an upstream outage, want 0", len(q))
	}
	runs, _ := h.st.RecentPollRuns(context.Background(), 1)
	if runs[0].Status != store.PollFailed || runs[0].Evaluated {
		t.Errorf("run = %+v, want failed and unevaluated", runs[0])
	}
	// The stored view must be untouched, so nothing is "learned" from a
	// failed poll.
	tg, err := h.st.Target(context.Background(), "node", key(0))
	if err != nil {
		t.Fatal(err)
	}
	if !tg.LastSeen.Equal(t0.Add(-time.Minute)) {
		t.Errorf("LastSeen = %v, want it unchanged by the failed poll", tg.LastSeen)
	}
}

// A truncated-but-parseable response is the sneakier version of the same
// failure, and must also be refused.
func TestTruncatedFeedNotifiesNobody(t *testing.T) {
	h := newHarness(t, nil)
	h.scope.setNodes(h.now, 400, time.Minute)
	h.poll(t)
	h.watch(t, key(0), 6)

	h.now = t0.Add(10 * time.Hour)
	h.scope.setNodes(h.now, 5, time.Minute) // way below both floors
	h.poll(t)

	if q := h.queued(t); len(q) != 0 {
		t.Fatalf("queued %d on a truncated feed, want 0", len(q))
	}
	runs, _ := h.st.RecentPollRuns(context.Background(), 1)
	if runs[0].Status != store.PollSuspect {
		t.Errorf("status = %q, want suspect", runs[0].Status)
	}
}

// A feed that is complete and well-formed but frozen — CoreScope's own
// upstream having died — is the hardest of the three to spot, because
// everything looks healthy except that nothing ever moves.
func TestFrozenFeedStopsEvaluating(t *testing.T) {
	h := newHarness(t, nil)
	h.scope.setNodes(h.now, 200, time.Minute)
	h.poll(t)
	h.watch(t, key(0), 6)

	// Same timestamps, over and over, while wall-clock time advances past
	// the threshold.
	frozen := h.scope.nodes
	for i := 1; i <= 4; i++ {
		h.now = t0.Add(time.Duration(i) * 3 * time.Hour)
		h.scope.nodes = frozen
		h.poll(t)
	}

	if q := h.queued(t); len(q) != 0 {
		t.Fatalf("queued %d against a frozen feed, want 0", len(q))
	}
	runs, _ := h.st.RecentPollRuns(context.Background(), 1)
	if runs[0].Status != store.PollSuspect {
		t.Errorf("status = %q, want suspect once the feed stops advancing", runs[0].Status)
	}
}

// The catch-all. Whatever the cause, a mass simultaneous failure is far more
// likely to be our problem than everyone's, so it tells the operator and
// nobody else.
func TestBreakerWithholdsAMassAlert(t *testing.T) {
	var breakerReason string
	h := newHarness(t, func(c *config.Config) {
		c.Alerts.MaxNewAlertsPerPoll = 3
		c.Alerts.MaxNewAlertFraction = 0.9
	})
	h.pl.OnBreaker = func(ctx context.Context, reason string) { breakerReason = reason }

	h.scope.setNodes(h.now, 200, time.Minute)
	h.poll(t)

	// Ten users each watching a different node.
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		u, err := h.st.UpsertUser(ctx, fmt.Sprintf("d%d", i), "u", "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.st.CreateWatch(ctx, store.Watch{
			UserID: u.ID, TargetKind: "node", TargetKey: key(i), ThresholdHours: 6,
		}, 50); err != nil {
			t.Fatal(err)
		}
	}

	// Everything goes quiet at once, but the feed itself still advances, so
	// the freshness checks pass and only the breaker can catch this.
	h.now = t0.Add(8 * time.Hour)
	h.scope.setNodes(h.now, 200, 9*time.Hour)
	h.poll(t)

	if q := h.queued(t); len(q) != 0 {
		t.Fatalf("queued %d user messages, want 0 — the breaker should have withheld them", len(q))
	}
	if breakerReason == "" {
		t.Error("the operator must be told when the breaker trips; it is their only signal")
	}
	runs, _ := h.st.RecentPollRuns(context.Background(), 1)
	if runs[0].SuppressedReason == "" {
		t.Error("the poll run should record why it was suppressed")
	}
}

// ...but ordinary activity under the limits must still get through.
func TestBreakerAllowsANormalOutage(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.Alerts.MaxNewAlertsPerPoll = 5
		c.Alerts.MaxNewAlertFraction = 0.9
	})
	h.scope.setNodes(h.now, 200, time.Minute)
	h.poll(t)
	h.watch(t, key(0), 6)

	h.now = t0.Add(8 * time.Hour)
	h.scope.setNodes(h.now, 200, time.Minute)
	h.scope.nodes[0]["last_seen"] = t0.Add(-time.Hour).Format(time.RFC3339)
	h.poll(t)

	if q := h.queued(t); len(q) != 1 {
		t.Fatalf("queued %d, want 1 — a single node failing is normal", len(q))
	}
}

// A watch added to a node that is already quiet must not produce an alert.
// Given 526 of 779 targets on the live instance have not been seen in over a
// day, this is the common case.
func TestWatchingAnAlreadyQuietNodeIsSilent(t *testing.T) {
	h := newHarness(t, nil)
	h.scope.setNodes(h.now, 200, time.Minute)
	h.scope.nodes[0]["last_seen"] = t0.Add(-72 * time.Hour).Format(time.RFC3339)
	h.poll(t)

	h.watch(t, key(0), 6)

	h.now = t0.Add(5 * time.Minute)
	h.scope.setNodes(h.now, 200, time.Minute)
	h.scope.nodes[0]["last_seen"] = t0.Add(-72 * time.Hour).Format(time.RFC3339)
	h.poll(t)

	if q := h.queued(t); len(q) != 0 {
		t.Fatalf("queued %v, want nothing for a node that was already down", q)
	}
}

// Restarting after downtime must not produce a backlog: state is derived
// from current timestamps, so there is nothing to replay.
func TestNoBacklogAfterDowntime(t *testing.T) {
	h := newHarness(t, nil)
	h.scope.setNodes(h.now, 200, time.Minute)
	h.poll(t)
	h.watch(t, key(0), 6)

	// Twelve hours pass with no polls at all, then the node is found quiet.
	h.now = t0.Add(12 * time.Hour)
	h.scope.setNodes(h.now, 200, time.Minute)
	h.scope.nodes[0]["last_seen"] = t0.Format(time.RFC3339)
	h.poll(t)

	q := h.queued(t)
	if len(q) != 1 {
		t.Fatalf("queued %d after downtime, want exactly 1", len(q))
	}
}

func TestPollWithNoWatchesStillRecordsTargets(t *testing.T) {
	h := newHarness(t, nil)
	h.scope.setNodes(h.now, 150, time.Minute)
	h.poll(t)

	n, err := h.st.CountTargets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 150 {
		t.Errorf("targets = %d, want 150", n)
	}
	runs, _ := h.st.RecentPollRuns(context.Background(), 1)
	if runs[0].Status != store.PollOK {
		t.Errorf("status = %q, want ok", runs[0].Status)
	}
}
