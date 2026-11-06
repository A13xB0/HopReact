package notify

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"hopreact/internal/poller"
	"hopreact/internal/store"
)

// queueBattery queues one battery alert with a draining week of readings.
func queueBattery(t *testing.T, st *store.Store, userID int64, level, name string, samples int) {
	t.Helper()
	var pts [][2]int64
	for i := 0; i < samples; i++ {
		at := t0.Add(-time.Duration(samples-i) * time.Hour)
		pts = append(pts, [2]int64{at.Unix(), int64(3950 - i*2)})
	}
	payload, _ := json.Marshal(poller.BatteryPayload{
		Kind: poller.KindBattery, Level: level, TargetKey: "cd6ec3" + name, TargetName: name,
		MV: 3620, ReadUnix: t0.Add(-time.Hour).Unix(), Reporter: "WMF RakPi OH",
		LowMV: 3650, CriticalMV: 3500, Samples: pts,
	})
	if err := st.WriteTx(context.Background(), func(tx *sql.Tx) error {
		return st.QueueNotification(context.Background(), tx, userID, poller.KindBattery, string(payload), time.Time{})
	}); err != nil {
		t.Fatal(err)
	}
}

func TestBatteryAlertCarriesChartCard(t *testing.T) {
	st, rec, n, _ := harness(t)
	n.ScopeURL = "https://corescope.example"
	ctx := context.Background()
	u, _ := st.UpsertUser(ctx, "d1", "alice", "")
	queueBattery(t, st, u.ID, "low", "Bowden_R63", 7*24)
	queue(t, st, u.ID, "alert", "Ben Nevis", "seen")

	if _, err := n.DrainOnce(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if len(rec.msgs) != 1 {
		t.Fatalf("sent %d messages, want the quiet-node and battery alerts in one", len(rec.msgs))
	}
	m := rec.msgs[0]
	for _, want := range []string{"Ben Nevis", "battery is getting low", "manage your alerts"} {
		if !strings.Contains(m.Content, want) {
			t.Errorf("content lacks %q:\n%s", want, m.Content)
		}
	}
	if strings.Count(m.Content, "manage your alerts") != 1 {
		t.Errorf("the link should appear once:\n%s", m.Content)
	}
	if len(m.Embeds) != 1 || len(m.Files) != 1 {
		t.Fatalf("embeds %d, files %d", len(m.Embeds), len(m.Files))
	}
	e := m.Embeds[0]
	if e.Image == nil || e.Image.URL != "attachment://"+m.Files[0].Name || !strings.HasPrefix(string(m.Files[0].Data), "\x89PNG") {
		t.Errorf("embed image %+v / file %q", e.Image, m.Files[0].Name)
	}
	if !strings.Contains(e.URL, "/#/nodes/cd6ec3Bowden_R63/analytics") || e.Color != colourLow {
		t.Errorf("embed %+v", e)
	}
	var fields []string
	for _, f := range e.Fields {
		fields = append(fields, f.Name+"="+f.Value)
	}
	joined := strings.Join(fields, "; ")
	if !strings.Contains(joined, "Battery=3.62 V · low is below 3.65 V") || !strings.Contains(joined, "Trend=") {
		t.Errorf("fields: %s", joined)
	}
}

func TestBatteryChartsCappedRestAsText(t *testing.T) {
	st, rec, n, _ := harness(t)
	ctx := context.Background()
	u, _ := st.UpsertUser(ctx, "d1", "alice", "")
	for i := 0; i < 6; i++ {
		queueBattery(t, st, u.ID, "critical", fmt.Sprintf("rep-%d", i), 48)
	}
	if _, err := n.DrainOnce(ctx, 10); err != nil {
		t.Fatal(err)
	}
	m := rec.msgs[0]
	if len(m.Embeds) != maxBatteryCharts || len(m.Files) != maxBatteryCharts {
		t.Fatalf("embeds %d files %d, want %d", len(m.Embeds), len(m.Files), maxBatteryCharts)
	}
	if !strings.Contains(m.Content, "6 repeaters' batteries are critical") ||
		!strings.Contains(m.Content, "**rep-4**") || !strings.Contains(m.Content, "**rep-5**") {
		t.Errorf("the rest should be text lines:\n%s", m.Content)
	}
}

func TestBatteryChartFailureFallsBackToText(t *testing.T) {
	st, rec, n, _ := harness(t)
	ctx := context.Background()
	u, _ := st.UpsertUser(ctx, "d1", "alice", "")
	queueBattery(t, st, u.ID, "low", "No readings", 0) // nothing to draw
	if _, err := n.DrainOnce(ctx, 10); err != nil {
		t.Fatal(err)
	}
	m := rec.msgs[0]
	if len(m.Embeds) != 0 || !strings.Contains(m.Content, "**No readings** (repeater): 3.62 V, low is below 3.65 V.") {
		t.Fatalf("embeds %d, content:\n%s", len(m.Embeds), m.Content)
	}
}

func TestBatteryEnabledMessage(t *testing.T) {
	st, rec, n, _ := harness(t)
	ctx := context.Background()
	u, _ := st.UpsertUser(ctx, "d1", "alice", "")
	payload, _ := json.Marshal(poller.BatteryEnabledPayload{
		Kind: poller.KindBatteryEnabled, LowMV: 3650, CriticalMV: 3500,
		Repeaters: []poller.BatteryEnabledItem{{Name: "Cadham", MV: 3610, Level: "low"}, {Name: "Bowden", MV: 4050, Level: "ok"}},
	})
	if err := st.WriteTx(ctx, func(tx *sql.Tx) error {
		return st.QueueNotification(ctx, tx, u.ID, poller.KindBatteryEnabled, string(payload), time.Time{})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := n.DrainOnce(ctx, 10); err != nil {
		t.Fatal(err)
	}
	c := rec.msgs[0].Content
	for _, want := range []string{"now on for 2 of your repeaters", "**Bowden**: 4.05 V", "**Cadham**: 3.61 V, already low", "below 3.65 V, and again below 3.50 V"} {
		if !strings.Contains(c, want) {
			t.Errorf("lacks %q:\n%s", want, c)
		}
	}
}

func TestFitContentKeepsUnderDiscordsLimit(t *testing.T) {
	var lines []string
	for i := 0; i < 200; i++ {
		lines = append(lines, fmt.Sprintf("• **repeater-%03d** (repeater): 3.62 V, low is below 3.65 V.", i))
	}
	tail := "\n\nhttps://alerts.example/ — manage your alerts"
	got := fitContent(strings.Join(lines, "\n"), tail)
	if n := len([]rune(got)); n > maxContent {
		t.Fatalf("%d characters, over Discord's %d", n, maxContent)
	}
	if !strings.HasSuffix(got, tail) || !strings.Contains(got, "more lines, see the site.") {
		t.Errorf("cut message lost its link or its note:\n%s", got[len(got)-200:])
	}
	if short := fitContent("hello", tail); short != "hello"+tail {
		t.Errorf("short message changed: %q", short)
	}
}
