package notify

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"hopreact/internal/alert"
	"hopreact/internal/corescope"
	"hopreact/internal/poller"
	"hopreact/internal/store"
	"hopreact/internal/sun"
	"hopreact/internal/weather"
)

const wxLat, wxLon = 54.905, -1.45

var wxClouds = []float64{0.9, 0.2, 0.95, 0.5, 0.97, 0.4, 0.15, 0.8, 0.99, 0.6}

func wxSun(t time.Time) float64 {
	el := sun.Elevation(t, wxLat, wxLon)
	if el <= 0 {
		return 0
	}
	d := int(math.Floor(t.Sub(t0).Hours() / 24))
	c := wxClouds[((d%len(wxClouds))+len(wxClouds))%len(wxClouds)]
	return 900 * math.Pow(math.Sin(el*math.Pi/180), 1.15) * (1 - 0.75*c*c)
}

// A summer solar repeater on a small panel: it needs a bright day to keep up.
func wxSamples() [][2]int64 { return wxSamplesFor(7) }

func wxSamplesFor(days int) [][2]int64 {
	var pts [][2]int64
	v := 3800.0
	for tm := t0.Add(-time.Duration(days) * 24 * time.Hour); !tm.After(t0); tm = tm.Add(10 * time.Minute) {
		v += -0.4 + 9*wxSun(tm.Add(5*time.Minute))/6/1000
		if tm.Minute() == 0 {
			pts = append(pts, [2]int64{tm.Unix(), int64(math.Round(v))})
		}
	}
	return pts
}

func wxServer(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		past, _ := strconv.Atoi(r.URL.Query().Get("past_days"))
		day0 := t0.Truncate(24 * time.Hour)
		var times []int64
		var temp, cloud, sw []float64
		for tm := day0.AddDate(0, 0, -past); tm.Before(day0.AddDate(0, 0, weather.ForecastDays)); tm = tm.Add(time.Hour) {
			var s float64
			for m := 5; m < 60; m += 10 {
				s += wxSun(tm.Add(-time.Hour + time.Duration(m)*time.Minute))
			}
			times, sw, cloud, temp = append(times, tm.Unix()), append(sw, s/6), append(cloud, 50), append(temp, 15)
		}
		json.NewEncoder(w).Encode(map[string]any{"hourly": map[string]any{
			"time": times, "temperature_2m": temp, "cloud_cover": cloud, "shortwave_radiation": sw}})
	}))
}

func TestBatteryAlertForecastAndLink(t *testing.T) {
	st, rec, n, _ := harness(t)
	ctx := context.Background()
	wx := wxServer(t)
	defer wx.Close()
	n.Weather = &weather.Service{Client: &weather.Client{BaseURL: wx.URL}, Cache: st, Now: n.Now}

	const key = "cd6ec34ee9759595e11e25dac9860a6c9f8df25a52d74a878ab228ad50d2447f"
	la, lo := wxLat, wxLon
	if _, err := st.UpsertTargets(ctx, []corescope.Observation{{Kind: corescope.KindNode, Key: key,
		Name: "Washington", Role: "repeater", LastSeen: t0, Lat: &la, Lon: &lo}}); err != nil {
		t.Fatal(err)
	}
	u, _ := st.UpsertUser(ctx, "d1", "alice", "")
	id, err := st.CreateWatch(ctx, store.Watch{UserID: u.ID, TargetKind: "node", TargetKey: key, ThresholdHours: 6}, 50)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(poller.BatteryPayload{Kind: poller.KindBattery, Level: "low", TargetKey: key,
		TargetName: "Washington", MV: 3640, ReadUnix: t0.Add(-time.Hour).Unix(), LowMV: 3650, CriticalMV: 3500,
		Samples: wxSamples()})
	if err := st.WriteTx(ctx, func(tx *sql.Tx) error {
		return st.QueueNotification(ctx, tx, u.ID, poller.KindBattery, string(payload), time.Time{})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := n.DrainOnce(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if len(rec.msgs) != 1 || len(rec.msgs[0].Embeds) != 1 {
		t.Fatalf("messages %d", len(rec.msgs))
	}
	fields := map[string]string{}
	for _, f := range rec.msgs[0].Embeds[0].Fields {
		fields[f.Name] = f.Value
	}
	if !strings.Contains(fields["Next 7 days"], "Expect") {
		t.Errorf("no forecast field; fields %v", fields)
	}
	want := "[Analyse battery](https://hopreact.example/watches/" + strconv.FormatInt(id, 10) + "/battery/analyse)"
	if fields["More"] != want {
		t.Errorf("More = %q, want %q", fields["More"], want)
	}
}

func TestBatteryAlertWithoutPositionOrWatch(t *testing.T) {
	st, rec, n, _ := harness(t)
	ctx := context.Background()
	u, _ := st.UpsertUser(ctx, "d1", "alice", "")
	queueBattery(t, st, u.ID, "low", "Bowden_R63", 7*24)
	if _, err := n.DrainOnce(ctx, 10); err != nil {
		t.Fatal(err)
	}
	for _, f := range rec.msgs[0].Embeds[0].Fields {
		if f.Name == "Next 7 days" || f.Name == "More" {
			t.Errorf("unexpected field %q without a position or a watch", f.Name)
		}
	}
}

func TestChartShading(t *testing.T) {
	var s []alert.Sample
	for i := 0; i < 7*24; i++ {
		s = append(s, alert.Sample{At: t0.Add(-time.Duration(7*24-i) * time.Hour), MV: 3900})
	}
	in := ChartInput{Name: "x", Samples: s, Now: t0, Levels: alert.BatteryLevels{LowMV: 3650, CriticalMV: 3500}}
	plain, err := BatteryChart(in)
	if err != nil {
		t.Fatal(err)
	}
	in.Spans = sun.Spans(t0.Add(-7*24*time.Hour), t0.Add(8*24*time.Hour), wxLat, wxLon)
	shaded, err := BatteryChart(in)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(plain, shaded) {
		t.Fatal("day and night shading changed nothing")
	}
}

// An alert carrying only 3 days can't forecast alone, but learns from the
// 30 days CoreScope has.
func TestBatteryAlertForecastLearnsFromCoreScope(t *testing.T) {
	st, rec, n, _ := harness(t)
	ctx := context.Background()
	wx := wxServer(t)
	defer wx.Close()
	n.Weather = &weather.Service{Client: &weather.Client{BaseURL: wx.URL}, Cache: st, Now: n.Now}
	const key = "cd6ec34ee9759595e11e25dac9860a6c9f8df25a52d74a878ab228ad50d2447f"
	cs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var out []map[string]any
		for _, p := range wxSamplesFor(30) {
			out = append(out, map[string]any{"timestamp": time.Unix(p[0], 0).UTC().Format(time.RFC3339), "battery_mv": p[1]})
		}
		json.NewEncoder(w).Encode(map[string]any{"status": "ok", "samples": out})
	}))
	defer cs.Close()
	n.Scope = corescope.NewClient(cs.URL, 5*time.Second, nil)

	la, lo := wxLat, wxLon
	if _, err := st.UpsertTargets(ctx, []corescope.Observation{{Kind: corescope.KindNode, Key: key,
		Name: "Washington", Role: "repeater", LastSeen: t0, Lat: &la, Lon: &lo}}); err != nil {
		t.Fatal(err)
	}
	u, _ := st.UpsertUser(ctx, "d1", "alice", "")
	short := wxSamples()
	short = short[len(short)-72:]
	payload, _ := json.Marshal(poller.BatteryPayload{Kind: poller.KindBattery, Level: "low", TargetKey: key,
		TargetName: "Washington", MV: 3640, ReadUnix: t0.Add(-time.Hour).Unix(), LowMV: 3650, CriticalMV: 3500,
		Samples: short})
	if err := st.WriteTx(ctx, func(tx *sql.Tx) error {
		return st.QueueNotification(ctx, tx, u.ID, poller.KindBattery, string(payload), time.Time{})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := n.DrainOnce(ctx, 10); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range rec.msgs[0].Embeds[0].Fields {
		found = found || (f.Name == "Next 7 days" && strings.Contains(f.Value, "Expect"))
	}
	if !found {
		t.Fatal("the forecast should learn from CoreScope's 30 days when the alert carries only 3")
	}
}
