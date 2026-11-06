package poller

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"hopreact/internal/config"
	"hopreact/internal/store"
)

// batteryOn turns battery alerts on with ScotMesh's levels and a check every
// poll, so tests don't have to wind the clock between checks.
func batteryOn(c *config.Config) {
	c.Battery.Enabled = true
	c.Battery.CheckEvery = time.Minute
	c.Battery.LowMV = 3650
	c.Battery.CriticalMV = 3500
}

// setBattery makes CoreScope report one reading for a node, read at `at`.
func (f *fakeScope) setBattery(k string, mv int, at time.Time) {
	if f.battery == nil {
		f.battery = map[string]map[string]any{}
	}
	status := "ok"
	switch {
	case mv == 0:
		status = "no_battery"
	case mv < 3000:
		status = "critical"
	case mv < 3300:
		status = "low"
	}
	f.battery[k] = map[string]any{
		"status": status, "latest_mv": mv, "latest_ts": at.Format(time.RFC3339), "stale": false,
		"selected":   map[string]any{"observer_name": "Cadham Village", "source": "neighbor"},
		"thresholds": map[string]any{"low_mv": 3300, "critical_mv": 3000},
		"samples":    []map[string]any{{"timestamp": at.Format(time.RFC3339), "battery_mv": mv}},
	}
}

// step moves the clock on an hour with a fresh feed and a new reading.
func (h *harness) step(t *testing.T, k string, mv int) {
	t.Helper()
	h.now = h.now.Add(time.Hour)
	h.scope.setNodes(h.now, 200, time.Minute)
	h.scope.setBattery(k, mv, h.now.Add(-5*time.Minute))
	h.poll(t)
}

func (h *harness) kinds(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, n := range h.queued(t) {
		out = append(out, n.Kind)
	}
	return out
}

func TestBatteryRuleAddedOnceWithOneSwitchOnMessage(t *testing.T) {
	h := newHarness(t, batteryOn)
	h.scope.setNodes(h.now, 200, time.Minute)
	h.poll(t)
	id := h.watch(t, key(0), 6)
	h.watch(t, key(1), 6) // no battery reported: never given a rule

	h.step(t, key(0), 3620)
	q := h.queued(t)
	if len(q) != 1 || q[0].Kind != KindBatteryEnabled {
		t.Fatalf("queued %v, want one switch-on message", h.kinds(t))
	}
	var p BatteryEnabledPayload
	if err := json.Unmarshal([]byte(q[0].Payload), &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Repeaters) != 1 || p.Repeaters[0].Level != "low" || p.LowMV != 3650 {
		t.Fatalf("payload %+v, want node-0 listed as already low against 3.65 V", p)
	}
	rule, has, err := h.st.BatteryRuleFor(context.Background(), id)
	if err != nil || !has || rule.Level != "low" {
		t.Fatalf("rule %+v has=%v err=%v", rule, has, err)
	}

	// Already low when added: said once in the switch-on message, not again.
	h.step(t, key(0), 3610)
	h.step(t, key(0), 3600)
	if k := h.kinds(t); len(k) != 1 {
		t.Fatalf("queued %v, want nothing more", k)
	}
	u, _ := h.st.UserByDiscordID(context.Background(), "d1")
	if u.BatteryBanner != 1 {
		t.Errorf("banner = %d, want 1", u.BatteryBanner)
	}
}

func TestBatteryRemovedRuleIsNeverAddedBack(t *testing.T) {
	h := newHarness(t, batteryOn)
	h.scope.setNodes(h.now, 200, time.Minute)
	h.poll(t)
	id := h.watch(t, key(0), 6)
	h.step(t, key(0), 4000)
	u, _ := h.st.UserByDiscordID(context.Background(), "d1")
	if err := h.st.DeleteBatteryRule(context.Background(), u.ID, id); err != nil {
		t.Fatal(err)
	}
	h.step(t, key(0), 4000)
	h.step(t, key(0), 4000)
	if _, has, _ := h.st.BatteryRuleFor(context.Background(), id); has {
		t.Fatal("a removed battery rule came back")
	}
	if k := h.kinds(t); len(k) != 1 {
		t.Fatalf("queued %v, want only the first switch-on message", k)
	}
}

func TestBatteryAutoOffAddsNothing(t *testing.T) {
	h := newHarness(t, batteryOn)
	h.scope.setNodes(h.now, 200, time.Minute)
	h.poll(t)
	id := h.watch(t, key(0), 6)
	u, _ := h.st.UserByDiscordID(context.Background(), "d1")
	if err := h.st.SetAutoBattery(context.Background(), u.ID, false); err != nil {
		t.Fatal(err)
	}
	h.step(t, key(0), 3600)
	if _, has, _ := h.st.BatteryRuleFor(context.Background(), id); has {
		t.Fatal("rule added with automatic battery alerts off")
	}
	if k := h.kinds(t); len(k) != 0 {
		t.Fatalf("queued %v", k)
	}
	if h.scope.batteryHits != 0 {
		t.Errorf("fetched %d batteries nobody can be given a rule for", h.scope.batteryHits)
	}
}

func TestBatteryLowThenCriticalThenOK(t *testing.T) {
	h := newHarness(t, batteryOn)
	h.scope.setNodes(h.now, 200, time.Minute)
	h.poll(t)
	h.watch(t, key(0), 6)
	h.step(t, key(0), 3900) // rule added at OK, switch-on message

	h.step(t, key(0), 3640)
	h.step(t, key(0), 3630) // second low reading: alert
	h.step(t, key(0), 3480)
	h.step(t, key(0), 3470) // critical
	h.step(t, key(0), 3900)
	h.step(t, key(0), 3900) // OK again

	var levels []string
	for _, n := range h.queued(t) {
		if n.Kind != KindBattery {
			continue
		}
		var p BatteryPayload
		if err := json.Unmarshal([]byte(n.Payload), &p); err != nil {
			t.Fatal(err)
		}
		levels = append(levels, p.Level)
		if p.LowMV != 3650 || p.CriticalMV != 3500 || len(p.Samples) == 0 || p.Reporter != "Cadham Village" {
			t.Errorf("payload %+v", p)
		}
	}
	if strings.Join(levels, ",") != "low,critical,recovered" {
		t.Fatalf("battery messages %v, want low, critical, recovered", levels)
	}
}

func TestBatteryUncheckedBoxOptsOut(t *testing.T) {
	h := newHarness(t, batteryOn)
	h.scope.setNodes(h.now, 200, time.Minute)
	h.poll(t)
	u, _ := h.st.UpsertUser(context.Background(), "d1", "alice", "")
	id, err := h.st.CreateWatch(context.Background(), store.Watch{
		UserID: u.ID, TargetKind: "node", TargetKey: key(0), NotifyRecovery: true, NoBattery: true,
	}, 50)
	if err != nil {
		t.Fatal(err)
	}
	h.step(t, key(0), 3600)
	if _, has, _ := h.st.BatteryRuleFor(context.Background(), id); has {
		t.Fatal("rule added to a watch whose owner unticked battery alerts")
	}
}

func TestBatteryFetchFailureChangesNothing(t *testing.T) {
	h := newHarness(t, batteryOn)
	h.scope.setNodes(h.now, 200, time.Minute)
	h.poll(t)
	id := h.watch(t, key(0), 6)
	h.step(t, key(0), 3900)
	h.step(t, key(0), 3640)

	// CoreScope stops knowing the node: nothing to judge, so no change.
	delete(h.scope.battery, key(0))
	h.now = h.now.Add(time.Hour)
	h.scope.setNodes(h.now, 200, time.Minute)
	h.poll(t)
	rule, _, _ := h.st.BatteryRuleFor(context.Background(), id)
	if rule.Level != "ok" || rule.CandidateCount != 1 {
		t.Fatalf("rule moved without a reading: %+v", rule)
	}
}

func TestBatteryOffFetchesNothing(t *testing.T) {
	h := newHarness(t, nil)
	h.scope.setNodes(h.now, 200, time.Minute)
	h.poll(t)
	h.watch(t, key(0), 6)
	h.step(t, key(0), 3600)
	if h.scope.batteryHits != 0 {
		t.Fatalf("fetched batteries with the feature off")
	}
}

func TestBatteryNoRecentReadingKeepsTheAlert(t *testing.T) {
	h := newHarness(t, batteryOn)
	h.scope.setNodes(h.now, 200, time.Minute)
	h.poll(t)
	id := h.watch(t, key(0), 6)
	h.step(t, key(0), 3900)
	h.step(t, key(0), 3480)
	h.step(t, key(0), 3470) // critical, announced

	// No reading in CoreScope's window: status "unknown", nothing to judge.
	h.now = h.now.Add(time.Hour)
	h.scope.setNodes(h.now, 200, time.Minute)
	h.scope.battery[key(0)] = map[string]any{"status": "unknown", "latest_mv": nil, "latest_ts": nil,
		"thresholds": map[string]any{"low_mv": 3300, "critical_mv": 3000}, "samples": []any{}}
	h.poll(t)
	rule, _, _ := h.st.BatteryRuleFor(context.Background(), id)
	if rule.Level != "critical" || rule.Announced != "critical" {
		t.Fatalf("a repeater that stopped reporting lost its alert: %+v", rule)
	}
}

func TestBatteryStaleReadingNotAutoAdded(t *testing.T) {
	h := newHarness(t, batteryOn)
	h.scope.setNodes(h.now, 200, time.Minute)
	h.poll(t)
	id := h.watch(t, key(0), 6)
	h.now = h.now.Add(time.Hour)
	h.scope.setNodes(h.now, 200, time.Minute)
	h.scope.setBattery(key(0), 3600, h.now.Add(-6*24*time.Hour))
	h.scope.battery[key(0)]["stale"] = true
	h.poll(t)
	if _, has, _ := h.st.BatteryRuleFor(context.Background(), id); has {
		t.Fatal("rule added from a six-day-old reading")
	}
}

func TestBatteryBreakerHoldsAlertsOnceAndSavesState(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		batteryOn(c)
		c.Alerts.MaxNewAlertsPerPoll = 2
	})
	trips := 0
	h.pl.OnBreaker = func(context.Context, string) { trips++ }
	h.scope.setNodes(h.now, 200, time.Minute)
	h.poll(t)
	for i := 0; i < 3; i++ {
		h.watch(t, key(i), 6)
	}
	all := func(mv int) {
		h.now = h.now.Add(time.Hour)
		h.scope.setNodes(h.now, 200, time.Minute)
		for i := 0; i < 3; i++ {
			h.scope.setBattery(key(i), mv, h.now.Add(-5*time.Minute))
		}
		h.poll(t)
	}
	all(3900) // rules added
	all(3600)
	all(3600) // three go low at once: over the limit of 2
	all(3600)
	all(3600)
	if trips != 1 {
		t.Fatalf("breaker tripped %d times, want once: the held state must be saved", trips)
	}
	for _, n := range h.queued(t) {
		if n.Kind == KindBattery {
			t.Fatalf("a battery alert went out through the breaker")
		}
	}
	// Held alerts were never announced, so their recovery is silent too.
	all(3900)
	all(3900)
	for _, n := range h.queued(t) {
		if n.Kind == KindBattery {
			t.Fatalf("recovery announced for alerts the breaker held")
		}
	}
}
