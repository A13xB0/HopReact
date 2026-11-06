package web

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"hopreact/internal/config"
	"hopreact/internal/corescope"
	"hopreact/internal/discord"
	"hopreact/internal/store"
	"hopreact/internal/sun"
	"hopreact/internal/weather"
)

// November, when solar repeaters in the north struggle.
var anNow = time.Date(2026, 11, 6, 20, 0, 0, 0, time.UTC)

const (
	anKey      = "38f6820f7e4dd88fed7697d822b552d141a3bd10ab45c239223f7e9aea6bba81"
	anLat      = 54.90475
	anLon      = -1.4499
	anFromDays = 35
)

// anNoTemps leaves the repeater's own temperature out of its readings.
var anNoTemps bool

// anAirTemp is the simulated air temperature; tests can make it freeze.
var anAirTemp = func(time.Time) float64 { return 8 }

var anClouds = []float64{0.9, 0.3, 0.95, 0.5, 0.97, 0.45, 0.18, 0.8, 0.99, 0.66}

func anCloud(t time.Time) float64 {
	d := int(math.Floor(t.Sub(anNow.Add(-anFromDays*24*time.Hour)).Hours() / 24))
	return anClouds[((d%len(anClouds))+len(anClouds))%len(anClouds)]
}

func anSun(t time.Time) float64 {
	el := sun.Elevation(t, anLat, anLon)
	if el <= 0 {
		return 0
	}
	c := anCloud(t)
	return 900 * math.Pow(math.Sin(el*math.Pi/180), 1.15) * (1 - 0.75*c*c)
}

// fakeScope serves a simulated solar repeater's battery, hourly.
func fakeScope(t *testing.T, fail *atomic.Bool) *httptest.Server {
	type sample struct {
		Timestamp   string   `json:"timestamp"`
		BatteryMV   int      `json:"battery_mv"`
		Temperature *float64 `json:"temperature_c,omitempty"`
	}
	var all []sample
	v := 3950.0
	for tm := anNow.Add(-anFromDays * 24 * time.Hour); !tm.After(anNow); tm = tm.Add(10 * time.Minute) {
		v += -0.29 + 45*anSun(tm.Add(5*time.Minute))/6/1000
		v = math.Min(v, 4200)
		if tm.Minute() == 0 {
			temp := 9 + 4*math.Sin(float64(tm.Hour()-9)/24*2*math.Pi)
			sm := sample{Timestamp: tm.Format(time.RFC3339), BatteryMV: int(math.Round(v))}
			if !anNoTemps {
				sm.Temperature = &temp
			}
			all = append(all, sm)
		}
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail != nil && fail.Load() {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		if !strings.Contains(r.URL.Path, anKey) {
			http.NotFound(w, r)
			return
		}
		days, _ := strconv.Atoi(r.URL.Query().Get("days"))
		from := anNow.Add(-time.Duration(days) * 24 * time.Hour)
		var out []sample
		for _, s := range all {
			if at, _ := time.Parse(time.RFC3339, s.Timestamp); !at.Before(from) {
				out = append(out, s)
			}
		}
		last := out[len(out)-1]
		json.NewEncoder(w).Encode(map[string]any{"status": "ok", "latest_mv": last.BatteryMV,
			"latest_ts": last.Timestamp, "samples": out})
	}))
}

// fakeWeather serves Open-Meteo's hourly shape from the same simulation.
func fakeWeather(t *testing.T, calls *atomic.Int32, fail *atomic.Bool) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if fail != nil && fail.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		q := r.URL.Query()
		if q.Get("latitude") != "54.9" || q.Get("longitude") != "-1.4" {
			t.Errorf("position sent to the weather service = %s, %s; want it rounded to 0.1°", q.Get("latitude"), q.Get("longitude"))
		}
		past, _ := strconv.Atoi(q.Get("past_days"))
		day0 := anNow.Truncate(24 * time.Hour)
		var times []int64
		var temp, cloud, sunW []float64
		for tm := day0.AddDate(0, 0, -past); tm.Before(day0.AddDate(0, 0, weather.ForecastDays)); tm = tm.Add(time.Hour) {
			var s float64
			for m := 5; m < 60; m += 10 {
				s += anSun(tm.Add(-time.Hour + time.Duration(m)*time.Minute))
			}
			times = append(times, tm.Unix())
			sunW = append(sunW, math.Round(s/6))
			cloud = append(cloud, math.Round(100*anCloud(tm.Add(-time.Minute))))
			temp = append(temp, anAirTemp(tm))
		}
		json.NewEncoder(w).Encode(map[string]any{"hourly": map[string]any{
			"time": times, "temperature_2m": temp, "cloud_cover": cloud, "shortwave_radiation": sunW}})
	}))
}

type anFixture struct {
	srv     *Server
	st      *store.Store
	h       http.Handler
	cookie  *http.Cookie
	watchID int64
	wxCalls *atomic.Int32
	wxFail  *atomic.Bool
	csFail  *atomic.Bool
}

func newAnalyse(t *testing.T, withPosition bool) anFixture {
	t.Helper()
	st, err := store.Open(t.TempDir(), func() time.Time { return anNow })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	f := anFixture{st: st, wxCalls: &atomic.Int32{}, wxFail: &atomic.Bool{}, csFail: &atomic.Bool{}}
	cs := fakeScope(t, f.csFail)
	t.Cleanup(cs.Close)
	wx := fakeWeather(t, f.wxCalls, f.wxFail)
	t.Cleanup(wx.Close)

	cfg := config.Default()
	cfg.Site.BaseURL = "http://localhost:8080"
	cfg.Discord = config.DiscordConfig{ClientID: "c", ClientSecret: "s", BotToken: "b", GuildID: "g"}
	cfg.Battery.Enabled, cfg.Battery.Weather = true, true
	cfg.Battery.LowMV, cfg.Battery.CriticalMV = 3650, 3500
	cfg.Battery.Timezone = "Europe/London"
	cfg.CoreScope.APIURL = cs.URL
	dc := discord.New("c", "s", "b", "g", cfg.Site.BaseURL+"/auth/callback", nil)
	srv, err := New(st, dc, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	srv.Scope = corescope.NewClient(cs.URL, 5*time.Second, nil)
	srv.Weather = &weather.Service{Client: &weather.Client{BaseURL: wx.URL}, Cache: st, Now: st.Now}
	f.srv, f.h = srv, srv.Routes()

	obs := corescope.Observation{Kind: corescope.KindNode, Key: anKey, Name: "G0BAZ Washington", Role: "repeater", LastSeen: anNow}
	if withPosition {
		la, lo := anLat, anLon
		obs.Lat, obs.Lon = &la, &lo
	}
	if _, err := st.UpsertTargets(context.Background(), []corescope.Observation{obs}); err != nil {
		t.Fatal(err)
	}
	cookie, _, u := signIn(t, srv, st)
	f.cookie = cookie
	f.watchID = addWatch(t, st, u.ID, anKey)
	if err := st.AddBatteryRule(context.Background(), u.ID, f.watchID); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f anFixture) get(t *testing.T, path string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.AddCookie(f.cookie)
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func (f anFixture) path(q string) string {
	return "/watches/" + itoa(f.watchID) + "/battery/analyse" + q
}

func TestAnalysePage(t *testing.T) {
	f := newAnalyse(t, true)
	code, body := f.get(t, f.path(""))
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, body)
	}
	for _, want := range []string{
		"Battery analysis", "G0BAZ Washington", "Position from its advert: 54.905, -1.450",
		`class="ph0"`, `class="ph4"`, `class="ac-sun"`, `class="ac-cloud"`, `class="ac-dev"`,
		"Day by day", "Sun to break even", "At a glance", "Brightest day", "Average night", "Next 7 days", "Open-Meteo.com",
		`aria-current="page">7 days`, "/static/analyse.js?v=",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page is missing %q", want)
		}
	}
	if n := strings.Count(body, "<tr>") + strings.Count(body, `<tr class="suspect"`); n < 8 {
		t.Errorf("ledger has %d rows, want a header and 7 days", n)
	}
}

func TestAnalyseRangeAndCache(t *testing.T) {
	f := newAnalyse(t, true)
	if code, _ := f.get(t, f.path("?days=7")); code != http.StatusOK {
		t.Fatal(code)
	}
	if code, _ := f.get(t, f.path("?days=3")); code != http.StatusOK {
		t.Fatal(code)
	}
	if n := f.wxCalls.Load(); n != 1 {
		t.Fatalf("weather fetched %d times; a shorter range should come from the cache", n)
	}
	code, body := f.get(t, f.path("?days=30"))
	if code != http.StatusOK || !strings.Contains(body, `aria-current="page">30 days`) {
		t.Fatalf("30 days: status %d", code)
	}
	if n := f.wxCalls.Load(); n != 1 {
		t.Fatalf("weather fetched %d times; every range learns from 30 days, so one fetch covers them all", n)
	}
	// Anything not offered falls back to a week.
	if _, body := f.get(t, f.path("?days=9999")); !strings.Contains(body, `aria-current="page">7 days`) {
		t.Fatal("an unknown range should show 7 days")
	}
}

func TestAnalyseWithoutPosition(t *testing.T) {
	f := newAnalyse(t, false)
	code, body := f.get(t, f.path(""))
	if code != http.StatusOK {
		t.Fatal(code)
	}
	if !strings.Contains(body, "doesn&#39;t share its position") || strings.Contains(body, `class="ph0"`) {
		t.Fatal("without a position the page should say so and draw no shading")
	}
	if f.wxCalls.Load() != 0 {
		t.Fatal("no position, no weather call")
	}
}

func TestAnalyseWeatherDown(t *testing.T) {
	f := newAnalyse(t, true)
	f.wxFail.Store(true)
	code, body := f.get(t, f.path(""))
	if code != http.StatusOK {
		t.Fatal(code)
	}
	if !strings.Contains(body, "Weather is unavailable") || !strings.Contains(body, `class="ph4"`) {
		t.Fatal("weather down: say so, keep the sun shading")
	}
	// Within the retry wait, no second call.
	f.get(t, f.path(""))
	if n := f.wxCalls.Load(); n != 1 {
		t.Fatalf("weather called %d times during an outage", n)
	}
}

func TestAnalyseCoreScopeDown(t *testing.T) {
	f := newAnalyse(t, true)
	f.csFail.Store(true)
	code, body := f.get(t, f.path(""))
	if code != http.StatusOK || !strings.Contains(body, "CoreScope didn&#39;t answer") {
		t.Fatalf("status %d; the page should fall back to stored readings and say so", code)
	}
}

func TestAnalyseOnlyYourOwnWatch(t *testing.T) {
	f := newAnalyse(t, true)
	other, err := f.st.UpsertUser(context.Background(), "d2", "bob", "")
	if err != nil {
		t.Fatal(err)
	}
	id := addWatch(t, f.st, other.ID, anKey)
	if code, _ := f.get(t, "/watches/"+itoa(id)+"/battery/analyse"); code != http.StatusNotFound {
		t.Fatalf("someone else's watch: status %d, want 404", code)
	}
}

func TestWatchPageLinksToAnalysis(t *testing.T) {
	f := newAnalyse(t, true)
	if err := f.st.SaveBatteryReadings(context.Background(), store.BatteryReadings{Key: anKey,
		Status: corescope.BatteryOK, LatestMV: 3940, LatestAt: anNow.Add(-time.Hour), FetchedAt: anNow,
		Samples: []corescope.BatterySample{{At: anNow.Add(-time.Hour), MV: 3940}}}); err != nil {
		t.Fatal(err)
	}
	_, body := f.get(t, "/watches/"+itoa(f.watchID))
	if !strings.Contains(body, `href="/watches/`+itoa(f.watchID)+`/battery/analyse">Analyse battery`) {
		t.Fatal("the battery card should link to the analysis")
	}
	_, body = f.get(t, "/watches")
	if !strings.Contains(body, `href="/watches/`+itoa(f.watchID)+`/battery/analyse">Analyse battery`) {
		t.Fatal("Your nodes should link to the analysis under the battery reading")
	}
}

// TestDumpAnalyse writes the page to $HOPREACT_DUMP for a look in a browser.
func TestDumpAnalyse(t *testing.T) {
	dir := os.Getenv("HOPREACT_DUMP")
	if dir == "" {
		t.Skip("set HOPREACT_DUMP to write pages")
	}
	f := newAnalyse(t, true)
	if err := f.st.SaveBatteryReadings(context.Background(), store.BatteryReadings{Key: anKey,
		Status: corescope.BatteryOK, LatestMV: 3940, LatestAt: anNow.Add(-time.Hour), FetchedAt: anNow,
		Samples: []corescope.BatterySample{{At: anNow.Add(-time.Hour), MV: 3940}}}); err != nil {
		t.Fatal(err)
	}
	_, nodes := f.get(t, "/watches")
	os.WriteFile(filepath.Join(dir, "watches.html"), []byte(nodes), 0o644)
	if os.Getenv("HOPREACT_COLD") != "" {
		anAirTemp = func(tm time.Time) float64 {
			if tm.After(anNow.Add(36 * time.Hour)) {
				return -4 + 3*math.Sin(float64(tm.Hour()-9)/24*2*math.Pi)
			}
			return 8
		}
		defer func() { anAirTemp = func(time.Time) float64 { return 8 } }()
		f = newAnalyse(t, true)
	}
	for name, q := range map[string]string{"analyse.html": "", "analyse-30.html": "?days=30", "analyse-fc.html": "?days=7&forecast=1"} {
		_, body := f.get(t, f.path(q))
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	js, _ := staticFS.ReadFile("static/analyse.js")
	os.MkdirAll(filepath.Join(dir, "static"), 0o755)
	os.WriteFile(filepath.Join(dir, "static", "analyse.js"), js, 0o644)
}

func TestAnalyseForecastToggle(t *testing.T) {
	f := newAnalyse(t, true)
	_, off := f.get(t, f.path(""))
	if !strings.Contains(off, `id="show-forecast" href="?days=7&amp;forecast=1" role="switch" aria-checked="false"`) {
		t.Fatal("the forecast switch should be there, off, and link to turn it on")
	}
	if strings.Contains(off, `class="ac-proj"`) || strings.Contains(off, `class="ac-nowline"`) {
		t.Fatal("no forecast drawn unless asked for")
	}
	code, on := f.get(t, f.path("?days=7&forecast=1"))
	if code != http.StatusOK {
		t.Fatal(code)
	}
	for _, want := range []string{`id="show-forecast" href="?days=7" role="switch" aria-checked="true"`, `class="ac-proj"`, `class="ac-band"`,
		`class="ac-nowline"`, "Forecast voltage", `href="?days=14&amp;forecast=1"`} {
		if !strings.Contains(on, want) {
			t.Errorf("forecast page is missing %q", want)
		}
	}
}

func TestAnalyseShortRangeStillForecasts(t *testing.T) {
	f := newAnalyse(t, true)
	_, body := f.get(t, f.path("?days=3&forecast=1"))
	if !strings.Contains(body, `class="ac-proj"`) || strings.Contains(body, "Not enough data to forecast") {
		t.Fatal("the 3-day view learns from 14 days, so it can forecast")
	}
}

func TestAnalyseSaysWhyNoForecast(t *testing.T) {
	f := newAnalyse(t, true)
	// A repeater whose readings only began three days ago.
	f.srv.Store.Now = func() time.Time { return anNow }
	cut := anNow.Add(-3 * 24 * time.Hour)
	f.srv.Scope = corescope.NewClient(fakeScopeFrom(t, cut).URL, 5*time.Second, nil)
	_, body := f.get(t, f.path("?days=7&forecast=1"))
	if strings.Contains(body, `class="ac-proj"`) {
		t.Fatal("no projection from 3 days")
	}
	for _, want := range []string{"Not enough data to forecast the battery yet.", "Not enough data to forecast yet.",
		"needs at least 5 full days of readings with weather", "only 2 so far", "Not enough data to forecast</text>"} {
		if !strings.Contains(body, want) {
			t.Errorf("page is missing %q", want)
		}
	}
	// Without the forecast ticked, the Next 7 days panel still says why.
	_, off := f.get(t, f.path("?days=7"))
	if !strings.Contains(off, "Not enough data to forecast yet.") || strings.Contains(off, "Not enough data to forecast the battery yet.") {
		t.Fatal("unticked: the panel explains, the chart note doesn't show")
	}
}

// fakeScopeFrom serves the simulated repeater with readings only from cut.
func fakeScopeFrom(t *testing.T, cut time.Time) *httptest.Server {
	inner := fakeScope(t, nil)
	t.Cleanup(inner.Close)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp, err := http.Get(inner.URL + r.URL.String())
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer resp.Body.Close()
		var body map[string]any
		json.NewDecoder(resp.Body).Decode(&body)
		var keep []any
		for _, x := range body["samples"].([]any) {
			at, _ := time.Parse(time.RFC3339, x.(map[string]any)["timestamp"].(string))
			if !at.Before(cut) {
				keep = append(keep, x)
			}
		}
		body["samples"] = keep
		json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A week with a gap in it has too few full days on its own, but the 7-day
// view learns from 30 days and still forecasts.
func TestAnalyseWeekWithGapStillForecasts(t *testing.T) {
	f := newAnalyse(t, true)
	inner := fakeScope(t, nil)
	t.Cleanup(inner.Close)
	gapFrom, gapTo := anNow.Add(-5*24*time.Hour), anNow.Add(-2*24*time.Hour)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp, err := http.Get(inner.URL + r.URL.String())
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer resp.Body.Close()
		var body map[string]any
		json.NewDecoder(resp.Body).Decode(&body)
		var keep []any
		for _, x := range body["samples"].([]any) {
			at, _ := time.Parse(time.RFC3339, x.(map[string]any)["timestamp"].(string))
			if at.Before(gapFrom) || at.After(gapTo) {
				keep = append(keep, x)
			}
		}
		body["samples"] = keep
		json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	f.srv.Scope = corescope.NewClient(srv.URL, 5*time.Second, nil)
	_, body := f.get(t, f.path("?days=7&forecast=1"))
	if !strings.Contains(body, `class="ac-proj"`) || strings.Contains(body, "Not enough data to forecast") {
		t.Fatal("the 7-day view should forecast from the longer history despite the gap")
	}
}

func TestAnalyseColdForecast(t *testing.T) {
	// Freezing air from tomorrow, and the repeater's own sensor runs only a
	// few degrees above the air, so the battery freezes too.
	anAirTemp = func(tm time.Time) float64 {
		if tm.After(anNow.Add(12 * time.Hour)) {
			return -6
		}
		return 8
	}
	defer func() { anAirTemp = func(time.Time) float64 { return 8 } }()
	f := newAnalyse(t, true)
	_, body := f.get(t, f.path("?days=7&forecast=1"))
	for _, want := range []string{`class="ac-frozen"`, `class="ac-frozen-tick"`, `class="ac-zero"`, `class="ac-dev-exp"`,
		"Too cold to charge (below 0 °C at the battery)", "Expected repeater temperature", "Too cold to charge at all on"} {
		if !strings.Contains(body, want) {
			t.Errorf("cold forecast page is missing %q", want)
		}
	}
	// Nothing is drawn left of the plot: the 30 days learned from aren't all shown.
	wide := body[strings.Index(body, `class="ac-lg"`):strings.Index(body, `class="ac-sm"`)]
	for _, line := range strings.Split(wide, `points="`)[1:] {
		x, _ := strconv.ParseFloat(strings.SplitN(strings.SplitN(line, ",", 2)[0], " ", 2)[0], 64)
		if x < chartLeft-0.5 && x > 0 {
			t.Fatalf("a line starts at x=%.1f, left of the plot at %.0f", x, chartLeft)
		}
	}
}

// Without the repeater's own temperatures the page still warns of frost from
// the air, but draws no "expected repeater temperature" line.
func TestAnalyseColdWithoutRepeaterTemps(t *testing.T) {
	anAirTemp = func(tm time.Time) float64 {
		if tm.After(anNow.Add(12 * time.Hour)) {
			return -6
		}
		return 8
	}
	defer func() { anAirTemp = func(time.Time) float64 { return 8 } }()
	anNoTemps = true
	defer func() { anNoTemps = false }()
	f := newAnalyse(t, true)
	_, body := f.get(t, f.path("?days=7&forecast=1"))
	if !strings.Contains(body, `class="ac-proj"`) || !strings.Contains(body, `class="ac-frozen"`) ||
		!strings.Contains(body, "from the air temperature") {
		t.Fatal("no repeater temperatures: still forecast, still warn of frost, and say it's from the air")
	}
	if strings.Contains(body, `class="ac-dev-exp"`) || strings.Contains(body, "Expected repeater temperature") {
		t.Fatal("no expected repeater temperature line without its readings")
	}
}
