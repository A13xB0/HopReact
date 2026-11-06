package batteryview

import (
	"strings"
	"testing"
	"time"

	"hopreact/internal/alert"
)

func TestWhen(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC) // a Friday
	day := 24 * time.Hour
	cases := []struct {
		e    alert.Estimate
		want string
	}{
		{alert.Estimate{Reached: true}, "now"},
		{alert.Estimate{Beyond: true}, "more than 2 months away"},
		{alert.Estimate{Soonest: 2 * day, Latest: 3 * day}, "in about 2–3 days, ~Monday"}, // midpoint Monday 00:00
		{alert.Estimate{Soonest: 36 * time.Hour, Latest: 60 * time.Hour}, "in about 2–3 days, ~Sunday"},
		{alert.Estimate{Soonest: 5 * time.Hour, Latest: 5 * time.Hour}, "in about 5 hours"},
		{alert.Estimate{Soonest: 20 * time.Minute, Latest: 20 * time.Minute}, "in about 1 hour"},
		{alert.Estimate{Soonest: 10 * day, Latest: 14 * day}, "in about 10–14 days"},
		{alert.Estimate{Soonest: 20 * day, Latest: alert.MaxEstimate + time.Hour}, "in at least 20 days"},
	}
	for _, c := range cases {
		if got := When(c.e, now, time.UTC); got != c.want {
			t.Errorf("When(%+v) = %q, want %q", c.e, got, c.want)
		}
	}
}

func TestAheadOfDraining(t *testing.T) {
	tr := alert.Trend{Kind: alert.TrendDraining, SlopeMVPerDay: -45, SlopeLow: -55, SlopeHigh: -40, LastLowMV: 3620}
	got := AheadOf(tr, alert.BatteryLevels{LowMV: 3650, CriticalMV: 3500}, time.Now(), time.UTC)
	// Already below low: only critical and the shut-off line are ahead.
	if len(got) != 2 || !strings.HasPrefix(got[0].Label, "Critical (3.50 V)") || !strings.HasPrefix(got[1].Label, "Likely shuts off") {
		t.Fatalf("AheadOf = %+v", got)
	}
	if Headline(tr) != "↓ Draining about 45 mV a day" {
		t.Errorf("Headline = %q", Headline(tr))
	}
}

func TestAheadOfChargingAndSteady(t *testing.T) {
	ch := alert.Trend{Kind: alert.TrendChargingNow, RateMVPerHour: 60, LatestMV: 3900}
	if got := AheadOf(ch, alert.BatteryLevels{}, time.Now(), time.UTC); len(got) != 1 || got[0].Value != "in about 5 hours" {
		t.Fatalf("charging now: %+v", got)
	}
	if got := AheadOf(alert.Trend{Kind: alert.TrendSteady}, alert.BatteryLevels{LowMV: 3650}, time.Now(), time.UTC); len(got) != 0 {
		t.Fatalf("steady has nothing ahead: %+v", got)
	}
}
