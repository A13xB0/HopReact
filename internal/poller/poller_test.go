package poller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
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
	// packets is the /api/packets page. Separate status so a test can break
	// just the packet feed while the node feed stays healthy.
	packets      []map[string]any
	packetStatus int
	packetHits   int
	srv          *httptest.Server
}

func newFakeScope(t *testing.T) *fakeScope {
	t.Helper()
	f := &fakeScope{status: http.StatusOK, packetStatus: http.StatusOK}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/packets", func(w http.ResponseWriter, r *http.Request) {
		f.packetHits++
		if f.packetStatus != http.StatusOK {
			http.Error(w, "no packets for you", f.packetStatus)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"packets": f.packets, "total": len(f.packets),
		})
	})
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
			"public_key":   key(i),
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

// key builds a distinct 64-hex public key for node i.
//
// The index goes at the FRONT deliberately. Padding it at the end — the
// obvious %064x — gives every node the same leading three bytes, so the
// attributor correctly refuses to resolve any of them and no per-type
// evidence is ever recorded. Real public keys differ from the first byte.
func key(i int) string { return fmt.Sprintf("%06x%058x", i, i) }

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

// ------------------------------------------------- per-type evidence ----

// advertPacket is an advert from key(i), which names its sender outright.
func advertPacket(id int64, i int, at time.Time) map[string]any {
	dj, _ := json.Marshal(map[string]any{"pubKey": key(i)})
	return map[string]any{
		"id": id, "first_seen": at.Format(time.RFC3339),
		"payload_type": corescope.TypeADVERT, "decoded_json": string(dj),
	}
}

// carriedPacket is traffic of the given type routed through key(i), which is
// identifiable only because the hop is three bytes wide.
func carriedPacket(id int64, typ, i int, at time.Time) map[string]any {
	return map[string]any{
		"id": id, "first_seen": at.Format(time.RFC3339),
		"payload_type": typ,
		"_parsedPath":  []string{strings.ToUpper(key(i)[:6])},
	}
}

// addRule attaches a per-type rule to a watch, which is how a user narrows
// what counts as their node being alive.
func (h *harness) addRule(t *testing.T, watchID int64, types []int, dir store.Direction, hours int) int64 {
	t.Helper()
	id, err := h.st.AddRule(context.Background(), 1, store.Rule{
		WatchID: watchID, Label: "test rule", Source: store.SourceTypes,
		Types: types, Direction: dir, ThresholdHours: hours,
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// The feature working: a node still being heard, but no longer carrying the
// kind of traffic the user cares about, alerts on that alone.
func TestPerTypeRuleAlertsWhenItsTrafficStops(t *testing.T) {
	h := newHarness(t, nil)
	h.scope.setNodes(h.now, 200, time.Minute) // everything heard a minute ago
	h.scope.packets = []map[string]any{
		carriedPacket(1, corescope.TypeGRPTXT, 0, h.now.Add(-time.Minute)),
	}
	id := h.watch(t, key(0), 6)
	h.addRule(t, id, []int{corescope.TypeGRPTXT}, store.DirCarried, 6)
	h.poll(t)
	if n := h.queued(t); len(n) != 0 {
		t.Fatalf("queued %d on a healthy node, want 0", len(n))
	}

	// Time passes. The node is still heard — the node feed keeps saying so —
	// but no more channel messages come through it.
	h.now = t0.Add(9 * time.Hour)
	h.scope.setNodes(h.now, 200, time.Minute)
	h.scope.packets = nil
	h.poll(t)

	got := h.queued(t)
	if len(got) != 1 {
		t.Fatalf("queued %d, want exactly 1 — the node is heard but not carrying", len(got))
	}
	if !strings.Contains(got[0].Payload, "test rule") {
		t.Errorf("the message should name the rule that tripped: %s", got[0].Payload)
	}
}

// THE guard. Our evidence only covers adverts and path hops at least three
// bytes wide — about 41% of packets — so a node can be perfectly healthy and
// still produce nothing we can attribute.
//
// A rule with no evidence must stay UNKNOWN. Asserting only that it sends
// nothing is too weak a test to be worth writing: the adopt-quiet rule
// already silences a first transition into alerting, so a broken
// implementation passes that check while telling the user on the dashboard
// that their node has been down all along. Claiming an outage we cannot see
// is the failure this guards against, so that is what is asserted.
func TestRuleWithNoEvidenceStaysUnknownAndSilent(t *testing.T) {
	h := newHarness(t, nil)
	id := h.watch(t, key(0), 6)
	// TRACE is real but vanishingly rare — one packet in three thousand on
	// the live mesh. Nothing will ever be recorded for it here.
	ruleID := h.addRule(t, id, []int{corescope.TypeTRACE}, store.DirCarried, 1)

	for i := 0; i < 6; i++ {
		h.now = t0.Add(time.Duration(i) * 12 * time.Hour)
		h.scope.setNodes(h.now, 200, time.Minute)
		// Plenty of other traffic, none of it TRACE.
		h.scope.packets = []map[string]any{
			carriedPacket(int64(100+i), corescope.TypeGRPTXT, 0, h.now.Add(-time.Minute)),
		}
		h.poll(t)
	}

	states, err := h.st.AllWatchState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := states[id][ruleID].State; got != store.StateUnknown {
		t.Errorf("state = %q, want %q — with no evidence we cannot claim it is down",
			got, store.StateUnknown)
	}
	for _, n := range h.queued(t) {
		if strings.Contains(n.Payload, "test rule") {
			t.Fatalf("a rule with no evidence must never alert, got %s", n.Payload)
		}
	}
}

// Adverts need no path hashes at all, so they are the one signal that cannot
// be starved by hop width. That is why the default rule set is built on them.
func TestAdvertsAttributeWithoutAnyPath(t *testing.T) {
	h := newHarness(t, nil)
	h.scope.setNodes(h.now, 200, time.Minute)
	h.scope.packets = []map[string]any{advertPacket(1, 0, h.now.Add(-2*time.Minute))}
	h.watch(t, key(0), 6)
	h.poll(t)

	acts, err := h.st.ActivityFor(context.Background(), "node", key(0))
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, a := range acts {
		if a.PayloadType == corescope.TypeADVERT && a.Direction == store.DirSent {
			found = true
			if a.EvidenceCount != 1 {
				t.Errorf("evidence count = %d, want 1", a.EvidenceCount)
			}
		}
	}
	if !found {
		t.Error("an advert should record its sender, with no path involved")
	}
}

// The packet feed is a sliding window that overlaps heavily between polls.
// Without the cursor, evidence_count would measure how often we polled rather
// than how much traffic there was.
func TestReplayedPacketsAreNotCountedTwice(t *testing.T) {
	h := newHarness(t, nil)
	h.scope.setNodes(h.now, 200, time.Minute)
	h.scope.packets = []map[string]any{
		advertPacket(1, 0, h.now.Add(-time.Minute)),
		advertPacket(2, 0, h.now.Add(-2*time.Minute)),
	}
	h.watch(t, key(0), 6)
	h.poll(t)

	// The same page again, as the real feed would serve it.
	h.now = t0.Add(5 * time.Minute)
	h.scope.setNodes(h.now, 200, time.Minute)
	h.poll(t)

	acts, _ := h.st.ActivityFor(context.Background(), "node", key(0))
	for _, a := range acts {
		if a.Direction == store.DirSent && a.EvidenceCount != 2 {
			t.Errorf("evidence count = %d after re-reading the same page, want 2", a.EvidenceCount)
		}
	}
}

// Losing the packet feed must not fail the poll. The seen and relayed signals
// are still perfectly good, and per-type rules should degrade to "no fresh
// evidence" rather than to "everything is offline".
func TestPacketFeedFailureDoesNotFailThePoll(t *testing.T) {
	h := newHarness(t, nil)
	h.scope.setNodes(h.now, 200, time.Minute)
	h.scope.packetStatus = http.StatusInternalServerError
	h.watch(t, key(0), 6)

	h.poll(t) // must not error

	runs, err := h.st.RecentPollRuns(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Status != store.PollOK {
		t.Fatalf("poll status = %+v, want ok despite the packet feed being down", runs)
	}
	if !runs[0].Evaluated {
		t.Error("the poll should still have evaluated its watches")
	}
	if h.scope.packetHits == 0 {
		t.Error("the packet feed should have been attempted")
	}
}
