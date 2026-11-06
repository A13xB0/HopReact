package alert

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

var london, _ = time.LoadLocation("Europe/London")

// series builds hourly readings ending at end: a daily swing of swing mV on
// top of a line falling or rising by perDay mV a day.
func series(end time.Time, days int, startMV, perDay, swing float64) []Sample {
	var out []Sample
	n := days * 24
	for i := 0; i <= n; i++ {
		at := end.Add(-time.Duration(n-i) * time.Hour)
		h := at.In(london).Hour()
		// Lowest just before dawn, highest mid-afternoon.
		day := 0.0
		if h >= 9 && h <= 16 {
			day = swing
		}
		mv := startMV + perDay*float64(i)/24 + day
		out = append(out, Sample{At: at, MV: int(mv)})
	}
	return out
}

func TestTrendRealWeekOfReadings(t *testing.T) {
	raw, err := os.ReadFile("testdata/battery_week.json")
	if err != nil {
		t.Fatal(err)
	}
	var data map[string][][2]int64
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	// What a person looking at each repeater's chart would say.
	want := map[string]TrendKind{
		"West Lomond ⛰️🌤":         TrendFull, // reached 4.18 V+ in the last day after recharging
		"Lucklaw Hill ⛰️🌤":        TrendSteady,
		"Alba-Éireann-01":         TrendSteady,
		"sco-fif-montrave":        TrendSteady,
		"sco-edi-garvald":         TrendSteady,
		"sco-earl-r65":            TrendSteady,
		"bosgag-tay-kilry-r":      TrendSteady,
		"Bishop Hill ☀️🔋":         TrendSteady,
		"sco-och-blairdenon":      TrendSteady,
		"WMF RAK-1 Test Rak 2":    TrendJumpy,
		"Waveshare MPPT - WMF":    TrendFull,
		"Heltec_Tower_V2 Repeate": TrendInsufficient, // a battery change mid-way restarts it
	}
	for name, pts := range data {
		var s []Sample
		for _, p := range pts {
			s = append(s, Sample{At: time.Unix(p[0], 0).UTC(), MV: int(p[1])})
		}
		now := s[len(s)-1].At.Add(10 * time.Minute)
		got := AnalyseTrend(s, now, london)
		if got.Kind == TrendDraining {
			t.Errorf("%s: called draining (%.0f mV/day) but nothing on the mesh was", name, got.SlopeMVPerDay)
		}
		if w, ok := want[name]; ok && got.Kind != w {
			t.Errorf("%s: %s (slope %.0f [%.0f..%.0f], nights %d, high %d), want %s",
				name, got.Kind, got.SlopeMVPerDay, got.SlopeLow, got.SlopeHigh, got.Nights, got.DailyHighMV, w)
		}
	}
}

func TestTrendDraining(t *testing.T) {
	end := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	for _, perDay := range []float64{-20, -60} {
		got := AnalyseTrend(series(end, 7, 3950, perDay, 40), end, london)
		if got.Kind != TrendDraining {
			t.Fatalf("%.0f mV/day: %s", perDay, got.Kind)
		}
		if got.SlopeMVPerDay > perDay*0.8 || got.SlopeMVPerDay < perDay*1.2 {
			t.Errorf("%.0f mV/day: slope %.1f", perDay, got.SlopeMVPerDay)
		}
		e := got.TimeTo(3500)
		want := float64(got.LastLowMV-3500) / -perDay
		if e.Reached || e.Beyond || e.Soonest.Hours()/24 > want*1.3 || e.Latest.Hours()/24 < want*0.7 {
			t.Errorf("%.0f mV/day: critical in %v..%v, want about %.1f days", perDay, e.Soonest, e.Latest, want)
		}
		if !got.TimeTo(4500).Reached {
			t.Errorf("a level above the battery should read as reached")
		}
	}
}

func TestTrendDrainingFarAwayIsBeyond(t *testing.T) {
	end := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	got := AnalyseTrend(series(end, 7, 4100, -11, 20), end, london)
	if got.Kind != TrendDraining {
		t.Fatalf("got %s", got.Kind)
	}
	if e := got.TimeTo(3300); !e.Beyond {
		t.Errorf("~70 days away should be beyond the two-month cap, got %v..%v", e.Soonest, e.Latest)
	}
}

func TestTrendSteadySolar(t *testing.T) {
	end := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	got := AnalyseTrend(series(end, 7, 4000, 0, 80), end, london)
	if got.Kind != TrendSteady {
		t.Fatalf("a big day/night swing with flat nights is steady, got %s (%.0f)", got.Kind, got.SlopeMVPerDay)
	}
	if !got.TimeTo(3500).Beyond {
		t.Error("a steady battery has no time to critical")
	}
}

func TestTrendChargingOverDays(t *testing.T) {
	end := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	got := AnalyseTrend(series(end, 6, 3800, 30, 30), end, london)
	if got.Kind != TrendCharging {
		t.Fatalf("got %s (%.0f [%.0f..%.0f])", got.Kind, got.SlopeMVPerDay, got.SlopeLow, got.SlopeHigh)
	}
	e := got.TimeToFull()
	if e.Reached || e.Beyond || e.Soonest <= 0 || e.Latest < e.Soonest {
		t.Errorf("full in %v..%v", e.Soonest, e.Latest)
	}
}

func TestTrendChargingNow(t *testing.T) {
	end := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	s := series(end.Add(-3*time.Hour), 4, 3700, 0, 0)
	for i, mv := range []int{3760, 3820, 3880} {
		s = append(s, Sample{At: end.Add(time.Duration(i-2) * time.Hour), MV: mv})
	}
	got := AnalyseTrend(s, end, london)
	if got.Kind != TrendChargingNow || got.RateMVPerHour < 50 {
		t.Fatalf("got %s at %.0f mV/h", got.Kind, got.RateMVPerHour)
	}
	if e := got.TimeToFull(); e.Soonest < 4*time.Hour || e.Soonest > 7*time.Hour {
		t.Errorf("full in %v, want about 5 h", e.Soonest)
	}
}

func TestTrendSwapRestartsHistory(t *testing.T) {
	end := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	s := series(end.Add(-24*time.Hour), 5, 3700, -40, 20)
	s = append(s, series(end, 1, 4120, 0, 10)...)
	got := AnalyseTrend(s, end, london)
	if got.Kind == TrendDraining {
		t.Fatalf("the old battery's fall carried over a swap")
	}
}

func TestTrendJumpyAndGaps(t *testing.T) {
	end := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	var s []Sample
	for i := 0; i < 72; i++ {
		mv := 4050
		if i%2 == 0 {
			mv = 4290
		}
		s = append(s, Sample{At: end.Add(-time.Duration(72-i) * time.Hour), MV: mv})
	}
	if got := AnalyseTrend(s, end, london); got.Kind != TrendJumpy {
		t.Errorf("hour-to-hour swings of 240 mV: %s", got.Kind)
	}

	// A 10-hour gap each day leaves too few readings on two of the days.
	gappy := series(end, 7, 3950, -40, 20)
	var kept []Sample
	for _, x := range gappy {
		if h := x.At.In(london).Hour(); h >= 2 && h < 12 {
			continue
		}
		kept = append(kept, x)
	}
	if got := AnalyseTrend(kept, end, london); got.Kind != TrendDraining {
		t.Errorf("gaps but enough readings each day: %s", got.Kind)
	}
}

func TestTrendNeedsThreeNights(t *testing.T) {
	end := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	if got := AnalyseTrend(series(end, 1, 3800, -100, 0), end, london); got.Kind != TrendInsufficient {
		t.Errorf("one day of readings: %s", got.Kind)
	}
	if got := AnalyseTrend(nil, end, london); got.Kind != TrendInsufficient {
		t.Errorf("no readings: %s", got.Kind)
	}
}
