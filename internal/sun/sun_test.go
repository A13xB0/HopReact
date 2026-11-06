package sun

import (
	"math"
	"testing"
	"time"
)

var london = time.FixedZone("BST", 3600)

func near(t *testing.T, what string, got, want time.Time, tol time.Duration) {
	t.Helper()
	if d := got.Sub(want); d > tol || d < -tol {
		t.Errorf("%s = %s, want %s (±%s)", what, got.Format("2006-01-02 15:04:05 MST"), want.Format("15:04 MST"), tol)
	}
}

// Almanac times (HM Nautical Almanac Office / timeanddate) for central
// London and Edinburgh.
func TestSunriseSunsetLondonMidsummer(t *testing.T) {
	d, ok := On(time.Date(2026, 6, 21, 12, 0, 0, 0, london), london, 51.5074, -0.1278)
	if !ok {
		t.Fatal("no sunrise")
	}
	near(t, "rise", d.Rise, time.Date(2026, 6, 21, 4, 43, 0, 0, london), 2*time.Minute)
	near(t, "set", d.Set, time.Date(2026, 6, 21, 21, 21, 0, 0, london), 2*time.Minute)
}

func TestSunriseSunsetLondonMidwinter(t *testing.T) {
	d, ok := On(time.Date(2026, 12, 21, 12, 0, 0, 0, time.UTC), time.UTC, 51.5074, -0.1278)
	if !ok {
		t.Fatal("no sunrise")
	}
	near(t, "rise", d.Rise, time.Date(2026, 12, 21, 8, 4, 0, 0, time.UTC), 2*time.Minute)
	near(t, "set", d.Set, time.Date(2026, 12, 21, 15, 53, 0, 0, time.UTC), 2*time.Minute)
}

func TestSunriseSunsetEdinburghMidwinter(t *testing.T) {
	d, ok := On(time.Date(2026, 12, 21, 12, 0, 0, 0, time.UTC), time.UTC, 55.9533, -3.1883)
	if !ok {
		t.Fatal("no sunrise")
	}
	near(t, "rise", d.Rise, time.Date(2026, 12, 21, 8, 42, 0, 0, time.UTC), 2*time.Minute)
	near(t, "set", d.Set, time.Date(2026, 12, 21, 15, 40, 0, 0, time.UTC), 2*time.Minute)
}

// In Scotland at midsummer the sun never gets 12° below the horizon, so the
// night is nautical twilight at its darkest.
func TestNoNightInScotlandAtMidsummer(t *testing.T) {
	from := time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC)
	for _, s := range Spans(from, from.Add(24*time.Hour), 57.4778, -4.2247) {
		if s.Phase == Night || s.Phase == Astronomical {
			t.Fatalf("Inverness at midsummer has %s from %s", s.Phase.Name(), s.Start)
		}
	}
}

func TestSpansCoverTheRangeInOrder(t *testing.T) {
	from := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(72 * time.Hour)
	sp := Spans(from, to, 54.905, -1.45)
	if !sp[0].Start.Equal(from) || !sp[len(sp)-1].End.Equal(to) {
		t.Fatalf("spans run %s to %s", sp[0].Start, sp[len(sp)-1].End)
	}
	seen := map[Phase]bool{}
	for i, s := range sp {
		seen[s.Phase] = true
		if i > 0 {
			if !s.Start.Equal(sp[i-1].End) {
				t.Fatalf("gap before span %d", i)
			}
			if s.Phase == sp[i-1].Phase {
				t.Fatalf("span %d repeats its neighbour's phase", i)
			}
			if d := int(s.Phase) - int(sp[i-1].Phase); d != 1 && d != -1 {
				t.Fatalf("span %d jumps from %s to %s", i, sp[i-1].Phase.Name(), s.Phase.Name())
			}
		}
	}
	if len(seen) != 5 {
		t.Fatalf("November in Newcastle should have all five phases, got %v", seen)
	}
}

func TestNoonElevation(t *testing.T) {
	// At the equinox the noon sun is 90° minus the latitude.
	var best float64 = -90
	from := time.Date(2026, 3, 20, 10, 0, 0, 0, time.UTC)
	for m := 0; m < 240; m++ {
		best = math.Max(best, Elevation(from.Add(time.Duration(m)*time.Minute), 55, 0))
	}
	if math.Abs(best-35) > 0.6 {
		t.Fatalf("noon elevation at 55°N on the equinox = %.2f, want about 35", best)
	}
}

func TestDaysNeedsBothEnds(t *testing.T) {
	from := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC) // after sunrise
	d := Days(from, from.Add(60*time.Hour), 54.905, -1.45)
	if len(d) != 2 {
		t.Fatalf("got %d days, want 2", len(d))
	}
	if d[0].Rise.Before(from) {
		t.Fatal("first day starts before the range")
	}
}
