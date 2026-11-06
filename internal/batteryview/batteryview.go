// Package batteryview puts battery readings and trends into words, the same
// way on the watch pages and in Discord messages.
package batteryview

import (
	"fmt"
	"math"
	"strings"
	"time"

	"hopreact/internal/alert"
)

// ShutOffMV is where most nRF repeaters shut themselves off: MeshCore refuses
// to boot them below 3.30 V, and Heltec V3/V4 boards fail around the same
// point when their 3.3 V supply can no longer keep up.
const ShutOffMV = 3300

// Volts renders millivolts as "3.62 V".
func Volts(mv int) string {
	return fmt.Sprintf("%.2f V", float64(mv)/1000)
}

// ShutOffDistinct reports whether the shut-off line is worth showing as well
// as the levels: not when it sits on, or above, low or critical.
func ShutOffDistinct(lv alert.BatteryLevels) bool {
	return ShutOffMV < lv.CriticalMV-50 && ShutOffMV < lv.LowMV-50
}

// Headline says which way a battery is going, in one line.
func Headline(t alert.Trend) string {
	switch t.Kind {
	case alert.TrendDraining:
		return fmt.Sprintf("↓ Draining about %s a day", mvRate(-t.SlopeMVPerDay))
	case alert.TrendCharging:
		return fmt.Sprintf("↑ Charging about %s a day", mvRate(t.SlopeMVPerDay))
	case alert.TrendChargingNow:
		if t.NearlyFull {
			return "↑ Charging now, nearly full"
		}
		return fmt.Sprintf("↑ Charging now, about %s an hour", mvRate(t.RateMVPerHour))
	case alert.TrendFull:
		return "✓ Full"
	case alert.TrendSteady:
		return "→ Holding steady"
	case alert.TrendJumpy:
		return "~ Too jumpy to estimate"
	}
	return "Needs a few more days of readings"
}

// Detail is the sentence under the headline.
func Detail(t alert.Trend) string {
	switch t.Kind {
	case alert.TrendDraining:
		return fmt.Sprintf("Night lows over the last %d nights. Voltage falls faster near the bottom, so treat the times as \"at most\".", t.Nights)
	case alert.TrendCharging:
		return fmt.Sprintf("Night lows rising over the last %d nights.", t.Nights)
	case alert.TrendChargingNow:
		return "Rising steadily over the last 3 hours: a charger, mains or a new battery."
	case alert.TrendFull:
		return "Reached 4.18 V or more in the last day. A nightly dip is normal on solar."
	case alert.TrendSteady:
		return fmt.Sprintf("Night lows within about 10 mV a day over %d nights.", t.Nights)
	case alert.TrendJumpy:
		return "Readings swing too much from one to the next for a reliable trend. Low and critical alerts still work."
	}
	return "A trend needs at least 3 days with 6 or more readings each."
}

// Ahead is one "how long until" line.
type Ahead struct {
	Name  string // "critical"
	Label string // "Critical (3.50 V)"
	Value string // "in about 2–3 days, ~Sunday"
}

// AheadOf lists how long until each level ahead: the levels below while
// draining (low, critical, and where it will likely shut itself off), full
// while charging.
func AheadOf(t alert.Trend, lv alert.BatteryLevels, now time.Time, loc *time.Location) []Ahead {
	var out []Ahead
	switch t.Kind {
	case alert.TrendDraining:
		for _, l := range []struct {
			name string
			mv   int
		}{{"Low", lv.LowMV}, {"Critical", lv.CriticalMV}, {"Likely shuts off", ShutOffMV}} {
			e := t.TimeTo(l.mv)
			if e.Reached || l.mv <= 0 || l.name == "Likely shuts off" && !ShutOffDistinct(lv) {
				continue
			}
			out = append(out, Ahead{Name: strings.ToLower(l.name), Label: fmt.Sprintf("%s (%s)", l.name, Volts(l.mv)), Value: When(e, now, loc)})
		}
	case alert.TrendCharging, alert.TrendChargingNow:
		e := t.TimeToFull()
		if !e.Reached {
			out = append(out, Ahead{Name: "full", Label: "Full (4.20 V)", Value: When(e, now, loc)})
		}
	}
	return out
}

// When renders an estimate as "in about 2–3 days, ~Sunday", "in about 5
// hours" or "more than 2 months away".
func When(e alert.Estimate, now time.Time, loc *time.Location) string {
	switch {
	case e.Reached:
		return "now"
	case e.Beyond:
		return "more than 2 months away"
	}
	if loc == nil {
		loc = time.UTC
	}
	lo, hi := e.Soonest, e.Latest
	if hi < lo {
		lo, hi = hi, lo
	}
	if hi > alert.MaxEstimate {
		return fmt.Sprintf("in at least %s", span(lo, lo))
	}
	s := "in about " + span(lo, hi)
	// Name the day when it's within the week, from the middle of the range.
	if mid := lo + (hi-lo)/2; mid >= 24*time.Hour && mid < 7*24*time.Hour {
		s += ", ~" + now.Add(mid).In(loc).Format("Monday")
	}
	return s
}

// span renders a range of durations in hours under two days, days after.
func span(lo, hi time.Duration) string {
	if hi < 48*time.Hour {
		a, b := roundAtLeast1(lo.Hours()), roundAtLeast1(hi.Hours())
		return rangeOf(a, b, "hour")
	}
	a, b := roundAtLeast1(lo.Hours()/24), roundAtLeast1(hi.Hours()/24)
	return rangeOf(a, b, "day")
}

func rangeOf(a, b int, unit string) string {
	if a == b {
		if a == 1 {
			return "1 " + unit
		}
		return fmt.Sprintf("%d %ss", a, unit)
	}
	return fmt.Sprintf("%d–%d %ss", a, b, unit)
}

func roundAtLeast1(v float64) int {
	n := int(math.Round(v))
	if n < 1 {
		return 1
	}
	return n
}

func mvRate(v float64) string {
	return fmt.Sprintf("%d mV", int(math.Round(v)))
}
