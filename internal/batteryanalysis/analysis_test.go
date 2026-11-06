package batteryanalysis

import (
	"math"
	"strings"
	"testing"
	"time"

	"hopreact/internal/alert"
	"hopreact/internal/sun"
	"hopreact/internal/weather"
)

const lat, lon = 54.905, -1.45 // Washington, Tyne and Wear

var (
	start = time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	now   = time.Date(2026, 11, 6, 20, 0, 0, 0, time.UTC)
	// Cloud per day: 31 Oct to 13 Nov. The second week is the forecast.
	clouds = []float64{0.92, 0.97, 0.45, 0.18, 0.8, 0.99, 0.66, 0.95, 0.9, 0.7, 0.98, 0.55, 0.35, 0.85}
	levels = alert.BatteryLevels{LowMV: 3650, CriticalMV: 3500}
)

// The simulated repeater draws loadPerStep every 10 minutes and gains
// gainPerKWh for each kWh/m² of sun, so its true break-even is
// loadPerDay / gainPerKWh.
const (
	loadPerStep = 0.29 // mV
	gainPerKWh  = 45.0 // mV
	loadPerDay  = loadPerStep * 6 * 24
)

func sunAt(t time.Time) float64 {
	el := sun.Elevation(t, lat, lon)
	if el <= 0 {
		return 0
	}
	day := int(t.Sub(start).Hours() / 24)
	c := clouds[min(day, len(clouds)-1)]
	return 900 * math.Pow(math.Sin(el*math.Pi/180), 1.15) * (1 - 0.75*c*c)
}

type sim struct {
	samples []alert.Sample
	hours   []weather.Hour
}

// simulate runs the repeater from start to now, readings hourly. panelDead
// stops charging from that time on (zero means never).
func simulate(v0 float64, panelDead time.Time) sim {
	var s sim
	v := v0
	step := 10 * time.Minute
	for t := start; !t.After(now); t = t.Add(step) {
		charging := panelDead.IsZero() || t.Before(panelDead)
		// Mean sun over this step, from its midpoint.
		w := sunAt(t.Add(step / 2))
		v -= loadPerStep
		if charging {
			v += gainPerKWh * w * step.Hours() / 1000
		}
		v = math.Min(v, 4200)
		if t.Minute() == 0 {
			s.samples = append(s.samples, alert.Sample{At: t, MV: int(math.Round(v))})
		}
	}
	for t := start.Add(time.Hour); t.Before(start.Add(14 * 24 * time.Hour)); t = t.Add(time.Hour) {
		var sum float64
		for m := 5; m < 60; m += 10 {
			sum += sunAt(t.Add(-time.Hour + time.Duration(m)*time.Minute))
		}
		day := int(t.Add(-time.Minute).Sub(start).Hours() / 24)
		s.hours = append(s.hours, weather.Hour{At: t, SunWm2: sum / 6, CloudPct: 100 * clouds[min(day, len(clouds)-1)], TempC: 8})
	}
	return s
}

func input(s sim) Input {
	return Input{Samples: s.samples, From: start, Now: now, HasPosition: true, Lat: lat, Lon: lon,
		Weather: s.hours, Levels: levels, Loc: time.UTC}
}

func TestLedgerAddsUp(t *testing.T) {
	r := Analyse(input(simulate(3995, time.Time{})))
	if len(r.Days) != 7 {
		t.Fatalf("got %d days, want 7 (31 Oct to 6 Nov)", len(r.Days))
	}
	complete := 0
	for _, d := range r.Days {
		if !d.HasWeather {
			t.Errorf("%s has no weather", d.Rise.Format("Mon 2"))
		}
		if d.HasNet {
			complete++
			if d.GainMV+d.OvernightMV != d.NetMV {
				t.Errorf("%s: gain %d + overnight %d != net %d", d.Rise.Format("Mon 2"), d.GainMV, d.OvernightMV, d.NetMV)
			}
			if d.OvernightMV >= 0 {
				t.Errorf("%s: overnight %d should be a loss", d.Rise.Format("Mon 2"), d.OvernightMV)
			}
		}
	}
	// The last day's next sunrise is tomorrow morning, still to come.
	if complete != 6 || r.Days[6].HasNet {
		t.Fatalf("complete days = %d, last day has net = %v", complete, r.Days[6].HasNet)
	}
	if !r.HasNet || r.NetMV >= 0 {
		t.Fatalf("a dull week should lose ground, net = %d", r.NetMV)
	}
}

func TestBreakEvenFindsTheTrueValue(t *testing.T) {
	r := Analyse(input(simulate(3995, time.Time{})))
	if r.Fit == nil {
		t.Fatalf("no fit: %s", r.NoFitReason)
	}
	want := loadPerDay / gainPerKWh
	if math.Abs(r.Fit.BreakEvenKWh-want) > 0.15 {
		t.Fatalf("break-even = %.2f kWh/m², want about %.2f", r.Fit.BreakEvenKWh, want)
	}
	if r.CompleteDays != 6 || r.BrokeEven < 1 || r.BrokeEven > 3 {
		t.Fatalf("broke even on %d of %d days", r.BrokeEven, r.CompleteDays)
	}
	v := r.Verdict(time.UTC)
	if v.Label != "Losing ground" || !strings.Contains(v.Text, "weather and the season") {
		t.Fatalf("verdict = %+v", v)
	}
}

func TestForecast(t *testing.T) {
	r := Analyse(input(simulate(3995, time.Time{})))
	if len(r.Forecast) != ForecastDays {
		t.Fatalf("forecast has %d days", len(r.Forecast))
	}
	for i, f := range r.Forecast {
		if f.LowMV > f.MidMV || f.HighMV < f.MidMV || f.HighMV > 4200 {
			t.Fatalf("day %d range %.0f / %.0f / %.0f out of order", i, f.LowMV, f.MidMV, f.HighMV)
		}
		if i > 0 && !f.Rise.After(r.Forecast[i-1].Rise) {
			t.Fatal("forecast days out of order")
		}
	}
	if !r.Forecast[0].Rise.Equal(r.StartAt) {
		t.Fatalf("forecast starts %s, its starting low is at %s", r.Forecast[0].Rise, r.StartAt)
	}
	txt := r.ForecastText(time.UTC)
	if !strings.Contains(txt, "too dull to break even") || !strings.Contains(txt, "Expect") {
		t.Fatalf("forecast text = %q", txt)
	}
}

func TestForecastWarnsBeforeLow(t *testing.T) {
	r := Analyse(input(simulate(3760, time.Time{})))
	if len(r.Crossings) == 0 {
		t.Fatalf("starting at %.0f mV a dull week should reach low; forecast %+v", r.StartMV, r.Forecast)
	}
	if r.Crossings[0].Level != "low" {
		t.Fatalf("first crossing = %+v", r.Crossings[0])
	}
	if !strings.Contains(r.ForecastText(time.UTC), "could be below low (3.65 V)") {
		t.Fatalf("forecast text = %q", r.ForecastText(time.UTC))
	}
}

func TestDeadPanel(t *testing.T) {
	// Sunny 2 and 3 Nov with the panel dead from 2 Nov.
	clouds[4], clouds[5] = 0.1, 0.1
	defer func() { clouds[4], clouds[5] = 0.8, 0.99 }()
	r := Analyse(input(simulate(3995, time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC))))
	if !r.PanelFault {
		for _, d := range r.Days {
			t.Logf("%s sun %.2f gain %d suspect %v", d.Rise.Format("Mon 2"), d.SunKWh, d.GainMV, d.PanelSuspect)
		}
		t.Fatal("panel fault not found")
	}
	if v := r.Verdict(time.UTC); v.Label != "Panel not charging" || v.Class != "bad" {
		t.Fatalf("verdict = %+v", v)
	}
}

func TestOneBrightDayWithoutGainIsNotAFault(t *testing.T) {
	r := Analyse(input(simulate(3995, time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC))))
	if r.PanelFault {
		t.Fatal("a single day can be a full battery or a shaded hour; it isn't a fault yet")
	}
}

func TestMains(t *testing.T) {
	var s []alert.Sample
	for t := start; t.Before(now); t = t.Add(time.Hour) {
		s = append(s, alert.Sample{At: t, MV: 4150 + int(t.Hour()%3)*5})
	}
	r := Analyse(Input{Samples: s, From: start, Now: now, HasPosition: true, Lat: lat, Lon: lon, Levels: levels})
	if !r.Mains || r.Fit != nil || len(r.Forecast) != 0 {
		t.Fatalf("mains = %v, fit = %v, forecast = %d", r.Mains, r.Fit, len(r.Forecast))
	}
	if v := r.Verdict(time.UTC); v.Label != "Mains or a big battery" {
		t.Fatalf("verdict = %+v", v)
	}
	if len(r.Facts(now, time.UTC, lat, lon)) != 0 {
		t.Fatal("no sunshine figures for a mains repeater")
	}
}

func TestNoPosition(t *testing.T) {
	in := input(simulate(3995, time.Time{}))
	in.HasPosition = false
	r := Analyse(in)
	if len(r.Days) != 0 || len(r.Spans) != 0 || r.Fit != nil {
		t.Fatal("without a position there are no days, spans or fit")
	}
	if !r.HasNet || r.Verdict(time.UTC).Label == "" {
		t.Fatal("the verdict still works from the readings alone")
	}
}

func TestNoWeather(t *testing.T) {
	in := input(simulate(3995, time.Time{}))
	in.Weather = nil
	r := Analyse(in)
	if r.Fit != nil || r.HasWeather || len(r.Forecast) != 0 {
		t.Fatal("no weather means no break-even or forecast")
	}
	if len(r.Days) != 7 || !r.Days[0].HasNet {
		t.Fatal("the ledger still works from sun times alone")
	}
}

func TestTooFewDays(t *testing.T) {
	in := input(simulate(3995, time.Time{}))
	in.From = now.Add(-72 * time.Hour)
	r := Analyse(in)
	if r.Fit != nil || !strings.Contains(r.NoFitReason, "5 full days") {
		t.Fatalf("fit = %v, reason %q", r.Fit, r.NoFitReason)
	}
}

func TestSeason(t *testing.T) {
	s, ok := Season(now, time.UTC, lat, lon)
	if !ok || s.Which != "Shortest" || s.Date.Month() != time.December {
		t.Fatalf("season = %+v", s)
	}
	if s.Change >= 0 || s.Change < -6*time.Minute {
		t.Fatalf("early November days shrink by about 4 minutes, got %s", s.Change)
	}
	if s.Length > 7*time.Hour+15*time.Minute || s.Length < 6*time.Hour+55*time.Minute {
		t.Fatalf("Newcastle's shortest day is about 7 hours, got %s", s.Length)
	}
}

func TestProjectKeepsTheDailyShape(t *testing.T) {
	s := simulate(3995, time.Time{})
	r := Analyse(input(s))
	until := now.Add(5 * 24 * time.Hour)
	p := r.Project(s.hours, until)
	if len(p) < 5*24 {
		t.Fatalf("projection has %d points, want about 5 days of hours", len(p))
	}
	if !p[0].At.Equal(r.LatestAt) || int(p[0].MidMV) != r.LatestMV {
		t.Fatal("projection should start at the latest reading")
	}
	rises, falls := 0, 0
	for i := 1; i < len(p); i++ {
		if p[i].At.After(until) {
			t.Fatal("projection runs past its end")
		}
		if p[i].LowMV > p[i].MidMV || p[i].HighMV < p[i].MidMV || p[i].HighMV > 4200 {
			t.Fatalf("point %d out of order", i)
		}
		switch {
		case p[i].MidMV > p[i-1].MidMV:
			rises++
		case p[i].MidMV < p[i-1].MidMV:
			falls++
		}
	}
	if rises < 10 || falls < 50 {
		t.Fatalf("rises %d, falls %d: should charge by day and drain by night", rises, falls)
	}
	// The daily morning projection and the hourly one agree to within the
	// spread of a day.
	end := p[len(p)-1]
	var morning ForecastDay
	for _, f := range r.Forecast {
		if f.Morning.Before(until) {
			morning = f
		}
	}
	if math.Abs(end.MidMV-morning.MidMV) > 60 {
		t.Fatalf("hourly ends at %.0f, daily says %.0f", end.MidMV, morning.MidMV)
	}
}

func TestProjectNeedsAFit(t *testing.T) {
	in := input(simulate(3995, time.Time{}))
	in.Weather = nil
	if p := Analyse(in).Project(simulate(3995, time.Time{}).hours, now.Add(48*time.Hour)); p != nil {
		t.Fatal("no fit, no projection")
	}
}

// A solar battery near full through a few dull days barely moves, which on
// its own looks like mains. Its history says otherwise.
func TestNearFullDullDaysAreNotMains(t *testing.T) {
	in := input(simulate(3995, time.Time{}))
	// The last three days flat at 4.12 V, as a full-ish cell in fog.
	for i := range in.Samples {
		if now.Sub(in.Samples[i].At) <= 72*time.Hour {
			in.Samples[i].MV = 4120 + (i%3)*5
		}
	}
	in.FitFrom = start
	in.From = now.Add(-72 * time.Hour)
	r := Analyse(in)
	if r.Mains {
		t.Fatal("a battery whose charge follows the sun isn't mains")
	}
	if r.Fit == nil {
		t.Fatalf("no fit: %s", r.NoFitReason)
	}
}

func TestForecastStartsFromAnHoursOldReading(t *testing.T) {
	s := simulate(3995, time.Time{})
	in := input(s)
	// Nothing in the last 9 hours, and no morning low since yesterday.
	in.Now = now.Add(9 * time.Hour)
	r := Analyse(in)
	if len(r.Forecast) == 0 || r.Project(s.hours, in.Now.Add(48*time.Hour)) == nil {
		t.Fatalf("a reading 9 hours old is still a fine start: %q", r.NoForecastReason())
	}
	in.Now = now.Add(30 * time.Hour)
	r = Analyse(in)
	if r.Project(s.hours, in.Now.Add(48*time.Hour)) != nil || !strings.Contains(r.NoForecastReason(), "more than a day old") {
		t.Fatalf("a day-old reading is too old: %q", r.NoForecastReason())
	}
}

// Readings every two hours with gaps, as Bishop Hill sends them, still fill
// the table. These are its real readings from 4 to 6 November 2026.
func TestTwoHourlyReadingsFillTheLedger(t *testing.T) {
	at := func(d, h, m int) time.Time { return time.Date(2026, 11, d, h, m, 0, 0, time.UTC) }
	var s []alert.Sample
	for _, r := range []struct{ d, h, m, mv int }{
		{3, 6, 40, 3930}, {3, 8, 40, 3940}, {3, 12, 40, 3960}, {3, 14, 40, 3965},
		{4, 0, 41, 3948}, {4, 2, 41, 3930}, {4, 4, 41, 3940}, {4, 6, 40, 3930}, {4, 8, 40, 3935}, {4, 18, 41, 3970},
		{5, 0, 40, 3970}, {5, 2, 42, 3970}, {5, 6, 41, 3960}, {5, 10, 41, 3950}, {5, 13, 3, 3960}, {5, 14, 40, 3950},
		{5, 16, 40, 3960}, {5, 20, 41, 3940}, {5, 22, 41, 3940},
		{6, 1, 3, 3950}, {6, 2, 40, 3930}, {6, 4, 40, 3920}, {6, 7, 3, 3930}, {6, 10, 41, 3910}, {6, 12, 40, 3900},
	} {
		s = append(s, alert.Sample{At: at(r.d, r.h, r.m), MV: r.mv})
	}
	const bLat, bLon = 56.215, -3.312
	r := Analyse(Input{Samples: s, From: at(3, 0, 0), Now: at(6, 21, 30), HasPosition: true, Lat: bLat, Lon: bLon, Levels: levels})
	got := map[int]Day{}
	for _, d := range r.Days {
		got[d.Rise.Day()] = d
	}
	wed, thu := got[4], got[5]
	if !wed.HasGain || !wed.HasOvernight || !wed.HasNet {
		t.Fatalf("Wed 4: gain %v overnight %v net %v", wed.HasGain, wed.HasOvernight, wed.HasNet)
	}
	if !thu.HasGain || !thu.HasOvernight || !thu.HasNet {
		t.Fatalf("Thu 5: gain %v overnight %v net %v", thu.HasGain, thu.HasOvernight, thu.HasNet)
	}
	if wed.MorningLowMV != 3930 || thu.MorningLowMV != 3960 || thu.NextLowMV != 3920 {
		t.Fatalf("morning lows %d, %d, %d", wed.MorningLowMV, thu.MorningLowMV, thu.NextLowMV)
	}
	if wed.PanelSuspect {
		t.Fatal("one reading at 08:40 and one at 18:41 isn't enough to call the panel dead")
	}
}

func TestFacts(t *testing.T) {
	r := Analyse(input(simulate(3995, time.Time{})))
	f := r.Facts(now, time.UTC, lat, lon)
	labels := map[string]Fact{}
	for _, x := range f {
		labels[x.Label] = x
	}
	for _, want := range []string{"Brightest day", "Dullest day", "Average night", "Tonight's dark", "Shortest day"} {
		if _, ok := labels[want]; !ok {
			t.Errorf("missing %q in %+v", want, f)
		}
	}
	if _, ok := labels["Break-even"]; ok {
		t.Error("break-even is in the headline figures once there is one")
	}
	if n := labels["Average night"]; n.Class != "bad" || !strings.HasPrefix(n.Value, "−") {
		t.Errorf("average night = %+v", n)
	}
	in := input(simulate(3995, time.Time{}))
	in.From = now.Add(-72 * time.Hour)
	be := Fact{}
	for _, x := range Analyse(in).Facts(now, time.UTC, lat, lon) {
		if x.Label == "Break-even" {
			be = x
		}
	}
	if be.Value != "Not yet" || !strings.Contains(be.Note, "needs 5 full days, has") {
		t.Errorf("no fit yet: %+v", be)
	}
}
