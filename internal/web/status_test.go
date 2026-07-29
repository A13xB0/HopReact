package web

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"hopreact/internal/corescope"
	"hopreact/internal/geo"
	"hopreact/internal/health"
	"hopreact/internal/store"
)

// Either signal resets the clock. A repeater that beacons but carries nothing,
// and one that carries traffic but has not beaconed, are both alive — and the
// board must say so.
func TestEitherSignalKeepsItGreen(t *testing.T) {
	now := t0
	fresh, stale := now.Add(-time.Minute), now.Add(-40*time.Hour)

	for _, c := range []struct {
		name            string
		traffic, advert time.Time
		want            string
	}{
		{"both fresh", fresh, fresh, "good"},
		{"only traffic is fresh", fresh, stale, "good"},
		{"only the advert is fresh", stale, fresh, "good"},
		{"both stale", stale, stale, "bad"},
		{"nothing at all", time.Time{}, time.Time{}, "muted"},
	} {
		got := judge(c.traffic, c.advert, now, 12, 24, 36)
		if got.Class != c.want {
			t.Errorf("%s: class = %q, want %q", c.name, got.Class, c.want)
		}
	}
}

// The ladder the operator configured has to be the ladder the board applies.
func TestEscalationLadder(t *testing.T) {
	now := t0
	for _, c := range []struct {
		ago   time.Duration
		class string
	}{
		{time.Hour, "good"},
		{11*time.Hour + 59*time.Minute, "good"},
		{12 * time.Hour, "amber"},
		{23 * time.Hour, "amber"},
		{24 * time.Hour, "orange"},
		{35 * time.Hour, "orange"},
		{36 * time.Hour, "bad"},
		{200 * time.Hour, "bad"},
	} {
		got := judge(now.Add(-c.ago), time.Time{}, now, 12, 24, 36)
		if got.Class != c.class {
			t.Errorf("%v old: class = %q, want %q", c.ago, got.Class, c.class)
		}
	}
}

// Every verdict explains itself, because the colour alone doesn't say which
// signal it rests on.
func TestVerdictExplainsItself(t *testing.T) {
	now := t0
	both := judge(now.Add(-2*time.Hour), now.Add(-time.Hour), now, 12, 24, 36)
	if !strings.Contains(both.Why, "advertised itself") || !strings.Contains(both.Why, "carrying traffic") {
		t.Errorf("both-signal hover should mention each: %q", both.Why)
	}
	noAdv := judge(now.Add(-time.Hour), time.Time{}, now, 12, 24, 36)
	if !strings.Contains(noAdv.Why, "never been heard advertising") {
		t.Errorf("missing-advert hover: %q", noAdv.Why)
	}
	none := judge(time.Time{}, time.Time{}, now, 12, 24, 36)
	if !strings.Contains(none.Why, "cannot see it, not that it is down") {
		t.Errorf("no-data hover must not imply the node is down: %q", none.Why)
	}
}

func statusServer(t *testing.T, tune func(*Server)) (*Server, *store.Store, http.Handler) {
	t.Helper()
	srv, st, h := newServer(t)
	srv.Cfg.Status.Enabled = true
	srv.Cfg.Status.Title = "Repeater status"
	srv.Cfg.Status.QuietHours = 12
	srv.Cfg.Status.ConcernHours = 24
	srv.Cfg.Status.AlarmHours = 36
	srv.Cfg.Status.RecentHours = 24
	srv.Cfg.Status.RecentLimit = 10
	if tune != nil {
		tune(srv)
	}
	return srv, st, h
}

// Off by default: an operator should decide to publish their mesh, not find
// they already have.
func TestStatusIsOffUnlessEnabled(t *testing.T) {
	_, _, h := newServer(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 when disabled", rec.Code)
	}
}

// And it needs no sign-in when it is on.
func TestStatusNeedsNoSignIn(t *testing.T) {
	_, _, h := statusServer(t, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for an anonymous visitor", rec.Code)
	}
}

func region(t *testing.T) *geo.Region {
	t.Helper()
	p := filepath.Join(t.TempDir(), "r.geojson")
	os.WriteFile(p, []byte(`{"type":"Feature","properties":{"name":"Testshire"},
	 "geometry":{"type":"Polygon","coordinates":[[[-5,55],[-2,55],[-2,58],[-5,58],[-5,55]]]}}`), 0o644)
	r, err := geo.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// The point of the region: on a bridged mesh most repeaters belong to
// somebody else, and listing them would make this their status board.
func TestOnlyRepeatersInsideTheRegionAreListed(t *testing.T) {
	srv, st, h := statusServer(t, nil)
	srv.Region = region(t)
	ctx := context.Background()
	in, out := 56.5, 53.5
	lon := -3.5
	if _, err := st.UpsertTargets(ctx, []corescope.Observation{
		{Kind: corescope.KindNode, Key: "aa", Name: "Inside Repeater", Role: "repeater",
			LastSeen: t0.Add(-time.Minute), Lat: &in, Lon: &lon},
		{Kind: corescope.KindNode, Key: "bb", Name: "Outside Repeater", Role: "repeater",
			LastSeen: t0.Add(-time.Minute), Lat: &out, Lon: &lon},
		{Kind: corescope.KindNode, Key: "cc", Name: "Nowhere Repeater", Role: "repeater",
			LastSeen: t0.Add(-time.Minute)},
		{Kind: corescope.KindNode, Key: "dd", Name: "A Companion", Role: "companion",
			LastSeen: t0.Add(-time.Minute), Lat: &in, Lon: &lon},
	}); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	body := rec.Body.String()

	if !strings.Contains(body, "Inside Repeater") {
		t.Error("a repeater inside the region should be listed")
	}
	if strings.Contains(body, "Outside Repeater") {
		t.Error("a repeater outside the region must not be listed")
	}
	if strings.Contains(body, "Nowhere Repeater") {
		t.Error("a repeater advertising no location cannot be placed in the region")
	}
	if strings.Contains(body, "A Companion") {
		t.Error("this is a repeater board; companions are not repeaters")
	}
	if !strings.Contains(body, "Testshire") {
		t.Error("the region should be named on the page")
	}
	// Groups are the point of the layout.
	for _, want := range []string{"Core repeaters", "Repeaters", "Observers"} {
		if !strings.Contains(body, want) {
			t.Errorf("the board should have a %q section", want)
		}
	}
}

// The board leads with a verdict and shows the colour it rests on.
func TestBoardShowsAVerdictAndHover(t *testing.T) {
	_, st, h := statusServer(t, nil)
	ctx := context.Background()
	if _, err := st.UpsertTargets(ctx, []corescope.Observation{
		{Kind: corescope.KindNode, Key: "aa", Name: "Dead Repeater", Role: "repeater",
			LastSeen: t0.Add(-100 * time.Hour)},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.WriteTx(ctx, func(tx *sql.Tx) error {
		return st.UpsertActivity(ctx, tx, "node", "aa", corescope.TypeADVERT,
			store.DirSent, t0.Add(-100*time.Hour), 3)
	}); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	body := rec.Body.String()

	// A repeater is not a critical group, so one dead one is a partial
	// outage rather than a major one.
	if !strings.Contains(body, "Partial outage") {
		t.Error("the headline should say what is wrong")
	}
	if !strings.Contains(body, "not active") {
		t.Error("the row should carry its verdict")
	}
	if !strings.Contains(body, "almost certainly stopped") {
		t.Error("the hover should explain why it is red")
	}
	// A board with something wrong must not claim otherwise.
	if strings.Contains(body, "All systems operational") {
		t.Error("the all-clear headline must not appear alongside a failure")
	}
}

// A backbone repeater is separated out, because losing one takes other
// repeaters with it. Thresholds follow CoreScope's own labelling.
func TestCoreRepeatersAreSeparated(t *testing.T) {
	srv, st, h := statusServer(t, nil)
	ctx := context.Background()
	lat, lon := 56.5, -3.5
	srv.Region = region(t)
	obs := []corescope.Observation{
		{Kind: corescope.KindNode, Key: "aa", Name: "Backbone Bridge", Role: "repeater",
			LastSeen: t0.Add(-time.Minute), Lat: &lat, Lon: &lon, BridgeScore: 0.31},
		{Kind: corescope.KindNode, Key: "bb", Name: "Backbone Traffic", Role: "repeater",
			LastSeen: t0.Add(-time.Minute), Lat: &lat, Lon: &lon, TrafficShare: 0.64},
		{Kind: corescope.KindNode, Key: "cc", Name: "Ordinary One", Role: "repeater",
			LastSeen: t0.Add(-time.Minute), Lat: &lat, Lon: &lon,
			BridgeScore: 0.04, TrafficShare: 0.08},
	}
	if _, err := st.UpsertTargets(ctx, obs); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	body := rec.Body.String()

	core := body[strings.Index(body, "Core repeaters"):]
	core = core[:strings.Index(core, "Observers")]
	for _, want := range []string{"Backbone Bridge", "Backbone Traffic"} {
		if !strings.Contains(core, want) {
			t.Errorf("%s should be in the core section", want)
		}
	}
	if strings.Contains(core[:strings.Index(core, "Repeaters</h2>")+1], "Ordinary One") {
		t.Error("a low-scoring repeater is not backbone")
	}
	// Either measure qualifies: they answer different questions.
	if !strings.Contains(body, "bridge 31%") || !strings.Contains(body, "traffic 64%") {
		t.Error("the board should show why a repeater counts as core")
	}
}

// A dead broker is a major outage; a dead repeater is not. Grouping only
// earns its keep if the headline reflects it.
func TestCriticalServiceFailureIsAMajorOutage(t *testing.T) {
	srv, _, h := statusServer(t, nil)
	srv.Health = &health.Monitor{Checks: []health.Check{
		{Name: "MQTT broker", Kind: "tcp", Target: "127.0.0.1:1"},
	}, Timeout: time.Second}
	srv.Health.Probe(contextWithTimeout(t))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	body := rec.Body.String()
	if !strings.Contains(body, "Major outage") {
		t.Error("a critical service being down is a major outage")
	}
	if !strings.Contains(body, "unreachable") {
		t.Error("the failing service should be marked unreachable")
	}
}

func contextWithTimeout(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)
	return ctx
}
