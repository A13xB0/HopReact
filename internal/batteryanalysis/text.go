package batteryanalysis

import (
	"fmt"
	"math"
	"time"

	"hopreact/internal/batteryview"
	"hopreact/internal/sun"
)

// Verdict is the one-line summary at the top of the page.
type Verdict struct {
	Label string
	// Class is the pill colour: good, warn, bad or muted.
	Class string
	Text  string
}

// Verdict sums the result up.
func (r Result) Verdict(loc *time.Location) Verdict {
	switch {
	case r.LatestMV == 0:
		return Verdict{"No readings", "muted", "There are no battery readings in this range."}
	case r.Mains:
		return Verdict{"Mains or a big battery", "good", fmt.Sprintf(
			"It has stayed at about %s with no dip at night, so it looks like mains power or a battery too big for a day to show. Daylight makes no difference to it.",
			batteryview.Volts(r.LatestMV))}
	case r.PanelFault:
		d := r.PanelDay
		return Verdict{"Panel not charging", "bad", fmt.Sprintf(
			"It had %.1f kWh/m² of sun on %s and gained only %d mV, and the day before was the same. Check the panel, its cable and the charge controller.",
			d.SunKWh, dayName(d.Rise, loc), d.GainMV)}
	case !r.HasNet:
		return Verdict{"Not enough readings", "muted", "There aren't enough readings in this range to say which way it's going."}
	}
	span := spanWords(r.NetSpan)
	var v Verdict
	switch {
	case r.NetMV < -steadyMV:
		v = Verdict{"Losing ground", "warn", fmt.Sprintf("Down %d mV over %s.", -r.NetMV, span)}
	case r.NetMV > steadyMV:
		v = Verdict{"Gaining", "good", fmt.Sprintf("Up %d mV over %s.", r.NetMV, span)}
	default:
		v = Verdict{"Holding steady", "good", fmt.Sprintf("Within %d mV of where it was %s ago.", steadyMV, span)}
	}
	if r.Fit != nil && r.CompleteDays > 0 {
		v.Text += fmt.Sprintf(" %d of %d days had enough sun to break even.", r.BrokeEven, r.CompleteDays)
		if r.NetMV < -steadyMV && r.chargesOnBrightDays() {
			v.Text += " It charges normally on brighter days, so this is the weather and the season rather than a fault."
		}
	}
	return v
}

// chargesOnBrightDays is set when the sunniest day in range gained well.
func (r Result) chargesOnBrightDays() bool {
	best, ok := r.sunniest()
	return ok && best.GainMV >= 2*panelGainMV
}

func (r Result) sunniest() (Day, bool) {
	var best Day
	ok := false
	for _, d := range r.Days {
		if d.HasGain && d.HasWeather && (!ok || d.SunKWh > best.SunKWh) {
			best, ok = d, true
		}
	}
	return best, ok
}

func (r Result) dullest() (Day, bool) {
	var worst Day
	ok := false
	for _, d := range r.Days {
		if d.HasGain && d.HasWeather && (!ok || d.SunKWh < worst.SunKWh) {
			worst, ok = d, true
		}
	}
	return worst, ok
}

// Fact is one figure in the "at a glance" grid: a label, the figure, and a
// short note under it. Class colours the figure: good, bad, or none.
type Fact struct {
	Label, Value, Note, Class string
}

// Facts lists the figures worth a glance, most useful first. The break-even
// only appears here while there isn't one, since the headline figures show
// it once there is.
func (r Result) Facts(now time.Time, loc *time.Location, lat, lon float64) []Fact {
	var out []Fact
	if r.Mains {
		return out
	}
	if r.Fit == nil && r.HasWeather && r.NoFitReason != "" {
		note := "needs more days of readings"
		switch r.NoFitReason {
		case ReasonFewDays:
			note = fmt.Sprintf("needs %d full days, has %d", minFitDays, r.FitDays)
		case ReasonSameSun:
			note = "needs a mix of bright and dull days"
		case ReasonNoFollow:
			note = "charge doesn't follow the sun"
		}
		out = append(out, Fact{Label: "Break-even", Value: "Not yet", Note: note})
	}
	mv := func(v int) (string, string) {
		switch {
		case v > 0:
			return fmt.Sprintf("+%d mV", v), "good"
		case v < 0:
			return fmt.Sprintf("−%d mV", -v), "bad"
		}
		return "0 mV", ""
	}
	best, okBest := r.sunniest()
	if okBest {
		v, c := mv(best.GainMV)
		out = append(out, Fact{Label: "Brightest day", Value: v, Class: c,
			Note: fmt.Sprintf("%s · %.2f kWh/m²", dayName(best.Rise, loc), best.SunKWh)})
	}
	if worst, ok := r.dullest(); ok && (!okBest || worst.Rise != best.Rise) {
		v, c := mv(worst.GainMV)
		out = append(out, Fact{Label: "Dullest day", Value: v, Class: c,
			Note: fmt.Sprintf("%s · %.2f kWh/m²", dayName(worst.Rise, loc), worst.SunKWh)})
	}

	var nightSum, nights int
	for _, d := range r.Days {
		if d.HasOvernight {
			nightSum += d.OvernightMV
			nights++
		}
	}
	if nights > 0 {
		v, c := mv(int(math.Round(float64(nightSum) / float64(nights))))
		out = append(out, Fact{Label: "Average night", Value: v, Class: c, Note: "from the day's high to dawn"})
	}
	if n := len(r.Days); n >= 2 {
		first, last := r.Days[0], r.Days[n-1]
		nf, nl := first.NextRise.Sub(first.Set), last.NextRise.Sub(last.Set)
		note := "about the same as " + dayName(first.Rise, loc)
		switch {
		case nl > nf+5*time.Minute:
			note = fmt.Sprintf("%s longer than %s", short(nl-nf), dayName(first.Rise, loc))
		case nl < nf-5*time.Minute:
			note = fmt.Sprintf("%s shorter than %s", short(nf-nl), dayName(first.Rise, loc))
		}
		out = append(out, Fact{Label: "Tonight's dark", Value: hm(nl), Note: note})
	}
	if s, ok := Season(now, loc, lat, lon); ok {
		out = append(out, Fact{Label: s.Which + " day", Value: s.Date.Format("2 Jan"),
			Note: fmt.Sprintf("%s of daylight, %s", hm(s.Length), moreLess(s.Length-s.Today))})
	}
	return out
}

// Seasonal is today's daylight and the next solstice's.
type Seasonal struct {
	Today, Change time.Duration // daylight today, and how much longer than yesterday
	Which         string        // "Shortest" or "Longest"
	Date          time.Time
	Length        time.Duration
}

// Season looks at today's daylight and the next solstice.
func Season(now time.Time, loc *time.Location, lat, lon float64) (Seasonal, bool) {
	if loc == nil {
		loc = time.UTC
	}
	today, ok1 := sun.On(now, loc, lat, lon)
	yday, ok2 := sun.On(now.AddDate(0, 0, -1), loc, lat, lon)
	if !ok1 || !ok2 {
		return Seasonal{}, false
	}
	l := now.In(loc)
	s := Seasonal{Today: today.Length(), Change: today.Length() - yday.Length()}
	june := time.Date(l.Year(), 6, 21, 12, 0, 0, 0, loc)
	dec := time.Date(l.Year(), 12, 21, 12, 0, 0, 0, loc)
	switch {
	case l.Before(june):
		s.Which, s.Date = "Longest", june
	case l.Before(dec):
		s.Which, s.Date = "Shortest", dec
	default:
		s.Which, s.Date = "Longest", june.AddDate(1, 0, 0)
	}
	d, ok := sun.On(s.Date, loc, lat, lon)
	if !ok {
		return Seasonal{}, false
	}
	s.Length = d.Length()
	return s, true
}

// NoForecastReason says, in plain words, why there's no battery forecast.
// Empty when there is one.
func (r Result) NoForecastReason() string {
	switch {
	case len(r.Forecast) > 0:
		return ""
	case r.Mains:
		return "It looks like mains power or a very big battery, so sunshine makes no difference to forecast."
	case r.LatestAt.IsZero():
		return "There are no readings to start a forecast from."
	case !r.HasWeather:
		return "There's no weather for this repeater to forecast from."
	case r.NoFitReason == ReasonFewDays:
		days := "none so far"
		switch {
		case r.FitDays == 1:
			days = "only 1 so far"
		case r.FitDays > 1:
			days = fmt.Sprintf("only %d so far", r.FitDays)
		}
		return fmt.Sprintf("It needs at least %d full days of readings with weather to learn how this battery responds to sunshine, and has %s.", minFitDays, days)
	case r.NoFitReason == ReasonSameSun:
		return "Every day so far has had much the same sunshine, so it can't yet tell how much the sun helps. A mix of bright and dull days will sort that out."
	case r.NoFitReason == ReasonNoFollow:
		return "Its charge doesn't follow the sunshine, which usually means a full or very big battery, mains power, or a panel that isn't charging."
	case r.Fit != nil:
		if r.age > maxStartAge {
			return "The newest reading is more than a day old, so there's nothing current to start a forecast from."
		}
		return "The weather forecast doesn't reach far enough ahead yet."
	}
	return "There isn't enough data yet."
}

// ForecastText is the coming week in a sentence or two, for the page and
// for alerts. Empty when there is no forecast.
func (r Result) ForecastText(loc *time.Location) string {
	if len(r.Forecast) == 0 {
		return ""
	}
	dull := 0
	for _, f := range r.Forecast {
		if f.NetMV < 0 {
			dull++
		}
	}
	n := len(r.Forecast)
	var text string
	switch {
	case dull == 0:
		text = fmt.Sprintf("The next %d days look bright enough to hold it.", n)
	case dull == n:
		text = fmt.Sprintf("None of the next %d days look bright enough to break even.", n)
	default:
		text = fmt.Sprintf("%d of the next %d days look too dull to break even.", dull, n)
	}
	last := r.Forecast[n-1]
	lo, hi := int(math.Round(last.LowMV)), int(math.Round(last.HighMV))
	if hi-lo < 20 {
		text += fmt.Sprintf(" Expect about %s by %s.", batteryview.Volts(int(math.Round(last.MidMV))), dayName(last.Morning, loc))
	} else {
		text += fmt.Sprintf(" Expect %.2f to %s by %s.", float64(lo)/1000, batteryview.Volts(hi), dayName(last.Morning, loc))
	}
	if len(r.Crossings) > 0 {
		c := r.Crossings[len(r.Crossings)-1] // low comes before critical when both
		for _, x := range r.Crossings {
			if x.Level == "low" {
				c = x
			}
		}
		text += fmt.Sprintf(" It could be below %s (%s) by %s morning.", c.Level, batteryview.Volts(c.MV), c.Day.In(locOr(loc)).Format("Monday"))
	}
	return text
}

func locOr(loc *time.Location) *time.Location {
	if loc == nil {
		return time.UTC
	}
	return loc
}

func dayName(t time.Time, loc *time.Location) string {
	return t.In(locOr(loc)).Format("Mon 2 Jan")
}

func hm(d time.Duration) string {
	m := int(math.Round(d.Minutes()))
	if m < 0 {
		m = -m
	}
	return fmt.Sprintf("%d h %02d m", m/60, m%60)
}

// short is a duration under an hour as minutes, otherwise as hm does.
func short(d time.Duration) string {
	if m := int(math.Round(math.Abs(d.Minutes()))); m < 60 {
		return fmt.Sprintf("%d min", m)
	}
	return hm(d)
}

func moreLess(d time.Duration) string {
	if d < 0 {
		return hm(d) + " less"
	}
	return hm(d) + " more"
}

func spanWords(d time.Duration) string {
	days := int(math.Round(d.Hours() / 24))
	switch {
	case days <= 1 && d < 36*time.Hour:
		return fmt.Sprintf("%d hours", int(math.Round(d.Hours())))
	default:
		return fmt.Sprintf("%d days", days)
	}
}
