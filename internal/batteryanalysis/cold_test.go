package batteryanalysis

import (
	"strings"
	"testing"
	"time"

	"hopreact/internal/corescope"
)

// A sunny day with the air below freezing all day.
func freeze(s sim, day time.Time, airC float64) {
	for i, h := range s.hours {
		if !h.At.Before(day) && h.At.Before(day.Add(24*time.Hour)) {
			s.hours[i].TempC = airC
		}
	}
}

func TestFrozenForecastDaysDontCharge(t *testing.T) {
	s := simulate(3995, time.Time{})
	warm := Analyse(input(s))
	// Sun 8 Nov freezes; the repeater's history has no temperatures, so the
	// air temperature stands for the battery.
	frozenDay := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
	freeze(s, frozenDay, -3)
	cold := Analyse(input(s))
	var fw, fc ForecastDay
	for i, f := range cold.Forecast {
		if f.Rise.Day() == 8 {
			fc, fw = f, warm.Forecast[i]
		}
	}
	if fc.Rise.IsZero() || fc.UsableKWh != 0 || fc.FrozenHours == 0 {
		t.Fatalf("Sun 8: usable %.2f, frozen hours %d", fc.UsableKWh, fc.FrozenHours)
	}
	if fc.NetMV >= fw.NetMV {
		t.Fatalf("a frozen day should net less than the same day warm: %.1f vs %.1f", fc.NetMV, fw.NetMV)
	}
	// No temperature readings from the repeater: the air stands in, and the
	// text says so.
	if txt := cold.ForecastText(time.UTC); !strings.Contains(txt, "Too cold to charge at all on Sunday") ||
		!strings.Contains(txt, "from the air temperature") {
		t.Fatalf("forecast text = %q", txt)
	}
	// The hourly projection doesn't rise on Sunday.
	for i, p := range cold.Project(s.hours, now.Add(5*24*time.Hour)) {
		if i > 0 && p.At.Day() == 8 && p.At.After(frozenDay) {
			prev := cold.Project(s.hours, now.Add(5*24*time.Hour))[i-1]
			if p.MidMV > prev.MidMV+0.01 {
				t.Fatalf("projection rises at %s on a frozen day", p.At)
			}
		}
	}
}

// Freezing air doesn't mean a freezing battery: a repeater that runs 8 °C
// above the air in daylight still charges at -3 °C.
func TestWarmEnclosureStillCharges(t *testing.T) {
	s := simulate(3995, time.Time{})
	in := input(s)
	for _, h := range s.hours {
		if h.At.Before(now) && h.SunWm2 > 0 {
			in.Temps = append(in.Temps, corescope.TempSample{At: h.At.Add(-30 * time.Minute), C: h.TempC + 8})
		}
	}
	freeze(s, time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC), -3)
	r := Analyse(in)
	if !r.HasTempOffset || r.TempOffsetC < 7.5 || r.TempOffsetC > 8.5 {
		t.Fatalf("offset = %.1f (%v), want about 8", r.TempOffsetC, r.HasTempOffset)
	}
	for _, f := range r.Forecast {
		if f.FrozenHours != 0 {
			t.Fatalf("%s: %d frozen hours, but the enclosure runs at about +5 °C", f.Rise.Format("Mon 2"), f.FrozenHours)
		}
	}
	if strings.Contains(r.ForecastText(time.UTC), "Too cold") {
		t.Fatal("no cold warning when the battery stays above 0 °C")
	}
	// With the repeater's own readings the warning, when there is one, is
	// based on them and doesn't hedge about the air.
	freeze(s, time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC), -12)
	r = Analyse(in)
	if txt := r.ForecastText(time.UTC); !strings.Contains(txt, "Too cold to charge") || strings.Contains(txt, "from the air temperature") {
		t.Fatalf("forecast text = %q", txt)
	}
}

// A sunny day that was frozen at the battery isn't a panel fault, and its
// sunshine doesn't count towards the break-even.
func TestFrozenPastDaysAreNotPanelFaults(t *testing.T) {
	// Panel dead isn't simulated: the battery simply didn't charge because it
	// was frozen on two bright days.
	clouds[4], clouds[5] = 0.1, 0.1
	defer func() { clouds[4], clouds[5] = 0.8, 0.99 }()
	s := simulate(3995, time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC))
	in := input(s)
	for _, d := range []int{2, 3, 4, 5, 6} {
		for i, h := range s.hours {
			if h.At.Day() == d && h.At.Month() == time.November {
				s.hours[i].TempC = -4
			}
		}
	}
	in.Weather = s.hours
	r := Analyse(in)
	if r.PanelFault {
		t.Fatal("frozen days can't charge; that's not the panel's fault")
	}
	for _, d := range r.Days {
		if d.Rise.Day() >= 2 && d.Rise.Day() <= 5 && d.HasWeather && d.UsableKWh != 0 {
			t.Fatalf("%s usable sun %.2f on a frozen day", d.Rise.Format("Mon 2"), d.UsableKWh)
		}
	}
}
