// Package batteryanalysis lines a repeater's battery readings up against
// daylight and weather: what each day gained and each night cost, how much
// sun it needs to break even, whether the panel is charging at all, and what
// the coming week's forecast means for it.
//
// Everything here is pure: readings, sun times and weather in, findings out.
package batteryanalysis

import (
	"math"
	"sort"
	"time"

	"hopreact/internal/alert"
	"hopreact/internal/corescope"
	"hopreact/internal/sun"
	"hopreact/internal/weather"
)

// Tunings, from a week of ScotMesh solar repeaters in November 2026.
const (
	// A solar battery bottoms out around sunrise, when the panel has given
	// nothing for hours. morningBefore/After is the window its low is taken
	// from, minMorning how many readings that window needs, and nearRise how
	// close to sunrise at least one of them must be. A battery drains all
	// night, so with readings only every couple of hours the night's lowest
	// still stands in well for the morning low.
	morningBefore = 5 * time.Hour
	morningAfter  = 3 * time.Hour
	minMorning    = 2
	nearRise      = 3 * time.Hour
	// The day's high is the highest reading from sunrise to highAfterSet
	// after sunset, and needs minDaytime readings.
	highAfterSet = 3 * time.Hour
	minDaytime   = 2
	// The panel check wants a well-covered day: at least panelReads readings
	// including one within midday of solar noon, so a missed peak can't
	// look like a dead panel.
	panelReads = 3
	midday     = 2 * time.Hour

	// minFitDays and minFitSpreadKWh: a break-even line needs enough days,
	// and days different enough in sunshine to tell the slope from noise.
	minFitDays      = 5
	minFitSpreadKWh = 0.4
	minSpreadMV     = 5

	// A day with at least panelSunKWh of sun that gains less than
	// panelGainMV, while the battery has room to charge, is suspicious. Two
	// in a row is a panel, cable or controller fault.
	panelSunKWh  = 1.0
	panelGainMV  = 10
	roomToFillMV = 4100

	// Readings that hold within mainsSwingMV for mainsWindow, above
	// mainsMinMV, are mains power or a battery too big for a day to show.
	mainsWindow  = 3 * 24 * time.Hour
	mainsSwingMV = 40
	mainsMinMV   = 4050
	mainsMinRead = 12

	// ForecastDays is how many days ahead are projected.
	ForecastDays = 7
	// steadyMV is how far the net change can move before a battery counts
	// as gaining or losing.
	steadyMV = 20
)

// Input is everything one analysis looks at.
type Input struct {
	Samples []alert.Sample
	// From and Now bound the range shown. Now is also where the forecast
	// starts.
	From, Now time.Time
	// HasPosition is false when the repeater doesn't advertise where it is;
	// then there is no daylight or weather to line up.
	HasPosition bool
	Lat, Lon    float64
	// Weather is hourly, past and forecast. Empty when weather is off or
	// unavailable.
	Weather []weather.Hour
	Levels  alert.BatteryLevels
	Loc     *time.Location
	// FitFrom, when before From, lets the break-even learn from more days
	// than are shown, so a short range can still be forecast.
	FitFrom time.Time
	// Temps are the repeater's own temperature readings, which tell when it
	// was too cold to charge and how much warmer than the air it runs.
	Temps []corescope.TempSample
}

// NoChargeBelowC is the temperature below which a lithium cell must not be
// charged; chargers stop, so sunshine in those hours gives nothing.
const NoChargeBelowC = 0.0

// minOffsetPairs is how many daylight readings of the repeater's own
// temperature it takes to trust how much warmer than the air it runs.
const minOffsetPairs = 6

// Day is one sunrise to the next: a day of charging and the night after it.
type Day struct {
	Rise, Set, NextRise time.Time
	// HasWeather is set when the weather covers the whole day.
	HasWeather bool
	SunKWh     float64 // sunlight energy on flat ground, kWh/m²
	CloudPct   float64 // mean cloud while the sun was up
	// UsableKWh is the sunshine in hours warm enough to charge, and
	// FrozenHours how many sunny hours were too cold.
	UsableKWh   float64
	FrozenHours int

	MorningLowMV int // lowest reading around this sunrise; 0 if unknown
	HighMV       int // highest from sunrise to after sunset; 0 if unknown
	NextLowMV    int // lowest around the next sunrise; 0 if unknown

	HasGain, HasOvernight, HasNet bool
	GainMV, OvernightMV, NetMV    int

	// PanelSuspect: plenty of sun, room to charge, and almost no gain.
	PanelSuspect bool
}

// Daylight is how long the sun was up.
func (d Day) Daylight() time.Duration { return d.Set.Sub(d.Rise) }

// Fit is the straight line from a day's sunshine to its net change.
type Fit struct {
	SlopeMVPerKWh float64
	InterceptMV   float64
	// BreakEvenKWh is the sunshine that holds the battery level. Zero or
	// less means it gains even on the dullest days seen.
	BreakEvenKWh float64
	// SpreadMV is how far a typical day lands from the line.
	SpreadMV float64
	Days     int
}

// NetFor is the expected net change for a day with sunKWh of sun.
func (f Fit) NetFor(sunKWh float64) float64 { return f.InterceptMV + f.SlopeMVPerKWh*sunKWh }

// ForecastDay is one projected day.
type ForecastDay struct {
	Rise time.Time
	// UsableKWh and FrozenHours as for Day: sun warm enough to charge in,
	// and sunny hours expected too cold.
	UsableKWh   float64
	FrozenHours int
	// Morning is the next sunrise, when the projected low applies.
	Morning time.Time
	SunKWh  float64
	NetMV   float64
	// MidMV is the projected low at the next sunrise, LowMV and HighMV its
	// likely range.
	MidMV, LowMV, HighMV float64
}

// Crossing is the first forecast day whose likely range reaches a level.
type Crossing struct {
	Level string // "low" or "critical"
	MV    int
	Day   time.Time // the morning it could be below
}

// Result is what the analysis found.
type Result struct {
	Spans []sun.Span
	Days  []Day
	// HasWeather is set when any day had full weather.
	HasWeather bool

	Fit *Fit
	// NoFitReason says why there is no break-even yet, and FitDays how
	// many usable days the break-even had to learn from.
	NoFitReason string
	FitDays     int
	// BrokeEven counts complete days at or above break-even.
	BrokeEven, CompleteDays int

	Forecast  []ForecastDay
	StartMV   float64
	StartAt   time.Time
	Crossings []Crossing

	Mains      bool
	PanelFault bool
	// PanelDay is the latest suspicious day, when PanelFault is set.
	PanelDay Day

	// NetMV is the change from the first morning low in range to the
	// latest, or first to latest reading without them, over NetSpan.
	HasNet  bool
	NetMV   int
	NetSpan time.Duration

	LatestMV int
	LatestAt time.Time
	// age is how old the latest reading was at Now.
	age time.Duration
	// cold estimates the battery's temperature hour by hour.
	cold coldModel
	// TempOffsetC is how much warmer than the air the repeater runs in
	// daylight, learned from its own readings, when HasTempOffset.
	TempOffsetC   float64
	HasTempOffset bool
}

// maxStartAge is the oldest a reading can be to start a forecast from.
const maxStartAge = 24 * time.Hour

// Analyse works through the readings.
func Analyse(in Input) Result {
	var r Result
	samples := within(in.Samples, in.From, in.Now)
	if n := len(samples); n > 0 {
		r.LatestMV, r.LatestAt = samples[n-1].MV, samples[n-1].At
		r.age = in.Now.Sub(r.LatestAt)
	}
	r.Mains = looksMains(samples, in.Now)
	r.HasNet, r.NetMV, r.NetSpan = netChange(samples)

	if !in.HasPosition {
		return r
	}
	r.Spans = sun.Spans(in.From, in.Now, in.Lat, in.Lon)
	// The break-even may learn from further back than the range shown.
	learn := in
	if !in.FitFrom.IsZero() && in.FitFrom.Before(in.From) {
		learn.From = in.FitFrom
	}
	r.cold = newColdModel(in.Temps, in.Weather)
	r.TempOffsetC, r.HasTempOffset = r.cold.offset, r.cold.hasOffset
	allDays := ledger(within(in.Samples, learn.From, in.Now), learn, r.cold)
	for _, d := range allDays {
		if !d.Set.Before(in.From) {
			r.Days = append(r.Days, d)
		}
		if d.HasNet && d.HasWeather {
			r.FitDays++
		}
	}
	if mid, span, ok := morningNet(r.Days); ok {
		r.NetMV, r.NetSpan, r.HasNet = mid, span, true
	}
	for _, d := range r.Days {
		r.HasWeather = r.HasWeather || d.HasWeather
	}
	r.PanelFault, r.PanelDay = panelFault(r.Days)
	r.Fit, r.NoFitReason = fit(allDays)
	// A few flat days near full can look like mains, but a battery whose
	// charge follows the sunshine over the longer history is solar.
	if r.Mains && r.Fit != nil {
		r.Mains = false
	}
	if r.Mains {
		r.Fit, r.NoFitReason = nil, ""
		return r
	}
	if r.Fit != nil {
		for _, d := range r.Days {
			if d.HasNet && d.HasWeather {
				r.CompleteDays++
				if d.UsableKWh >= r.Fit.BreakEvenKWh {
					r.BrokeEven++
				}
			}
		}
		forecast(&r, samples, in)
	}
	return r
}

func within(s []alert.Sample, from, to time.Time) []alert.Sample {
	var out []alert.Sample
	for _, x := range s {
		if x.MV > 0 && !x.At.Before(from) && !x.At.After(to) {
			out = append(out, x)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

// extreme returns the lowest or highest reading in [from, to] and how many
// readings there were.
func extreme(s []alert.Sample, from, to time.Time, highest bool) (int, int) {
	best, n := 0, 0
	for _, x := range s {
		if x.At.Before(from) || x.At.After(to) {
			continue
		}
		n++
		if n == 1 || (highest && x.MV > best) || (!highest && x.MV < best) {
			best = x.MV
		}
	}
	return best, n
}

func ledger(s []alert.Sample, in Input, c coldModel) []Day {
	// One day either side so the first day has its sunrise and the last its
	// next sunrise.
	all := sun.Days(in.From.Add(-36*time.Hour), in.Now.Add(36*time.Hour), in.Lat, in.Lon)
	var out []Day
	for i := 0; i+1 < len(all); i++ {
		d := Day{Rise: all[i].Rise, Set: all[i].Set, NextRise: all[i+1].Rise}
		// A day belongs to the range when its charging happened in it.
		if d.Set.Before(in.From) || d.Rise.After(in.Now) {
			continue
		}
		d.MorningLowMV = morningLow(s, d.Rise, in.Now)
		wellCovered := false
		if !in.Now.Before(d.Set.Add(highAfterSet)) {
			if mv, n := extreme(s, d.Rise, d.Set.Add(highAfterSet), true); n >= minDaytime {
				d.HighMV = mv
				noon := d.Rise.Add(d.Set.Sub(d.Rise) / 2)
				_, mid := extreme(s, noon.Add(-midday), noon.Add(midday), true)
				wellCovered = n >= panelReads && mid > 0
			}
		}
		d.NextLowMV = morningLow(s, d.NextRise, in.Now)
		if d.MorningLowMV > 0 && d.HighMV > 0 {
			d.HasGain, d.GainMV = true, d.HighMV-d.MorningLowMV
		}
		if d.HighMV > 0 && d.NextLowMV > 0 {
			d.HasOvernight, d.OvernightMV = true, d.NextLowMV-d.HighMV
		}
		if d.MorningLowMV > 0 && d.NextLowMV > 0 {
			d.HasNet, d.NetMV = true, d.NextLowMV-d.MorningLowMV
		}
		w := dayWeather(in.Weather, d, c)
		d.HasWeather, d.SunKWh, d.CloudPct, d.UsableKWh, d.FrozenHours = w.ok, w.sunKWh, w.cloud, w.usableKWh, w.frozen
		// A frozen battery can't charge, so cold days say nothing about the
		// panel.
		d.PanelSuspect = d.HasGain && wellCovered && d.HasWeather && d.UsableKWh >= panelSunKWh &&
			d.GainMV < panelGainMV && d.MorningLowMV < roomToFillMV
		out = append(out, d)
	}
	return out
}

// morningLow is the lowest reading through the night up to a few hours
// after sunrise, or 0 when there aren't enough readings near it.
func morningLow(s []alert.Sample, rise, now time.Time) int {
	if now.Before(rise.Add(morningAfter)) {
		return 0
	}
	mv, n := extreme(s, rise.Add(-morningBefore), rise.Add(morningAfter), false)
	if n < minMorning {
		return 0
	}
	if _, near := extreme(s, rise.Add(-nearRise), rise.Add(morningAfter), false); near == 0 {
		return 0
	}
	return mv
}

// dayWeather sums the sunshine from one sunrise to the next and averages
// the cloud while the sun was up.
type dayWx struct {
	ok                       bool
	sunKWh, usableKWh, cloud float64
	frozen                   int
}

func dayWeather(hours []weather.Hour, d Day, c coldModel) dayWx {
	var w dayWx
	var sunWh, usableWh, cloud float64
	n, nc := 0, 0
	for _, h := range hours {
		if !h.At.After(d.Rise) || h.At.After(d.NextRise) {
			continue
		}
		n++
		sunWh += h.SunWm2
		if c.frozen(h) {
			w.frozen++
		} else {
			usableWh += h.SunWm2
		}
		if !h.At.After(d.Set.Add(time.Hour)) {
			cloud += h.CloudPct
			nc++
		}
	}
	want := int(d.NextRise.Sub(d.Rise).Hours())
	if n == 0 || n < want-1 || nc == 0 {
		return dayWx{}
	}
	w.ok, w.sunKWh, w.usableKWh, w.cloud = true, sunWh/1000, usableWh/1000, cloud/float64(nc)
	return w
}

// morningNet is the change between the first and last known morning lows,
// which compares like with like where raw readings would mostly measure the
// time of day.
func morningNet(days []Day) (int, time.Duration, bool) {
	var first, last *Day
	for i := range days {
		if days[i].MorningLowMV == 0 {
			continue
		}
		if first == nil {
			first = &days[i]
		}
		last = &days[i]
	}
	// The newest morning low may be the next one after the last day.
	lastMV, lastAt := 0, time.Time{}
	if last != nil {
		lastMV, lastAt = last.MorningLowMV, last.Rise
		for i := range days {
			if days[i].NextLowMV > 0 && days[i].NextRise.After(lastAt) {
				lastMV, lastAt = days[i].NextLowMV, days[i].NextRise
			}
		}
	}
	if first == nil || !lastAt.After(first.Rise) {
		return 0, 0, false
	}
	return lastMV - first.MorningLowMV, lastAt.Sub(first.Rise), true
}

func netChange(s []alert.Sample) (bool, int, time.Duration) {
	if len(s) < 2 {
		return false, 0, 0
	}
	return true, s[len(s)-1].MV - s[0].MV, s[len(s)-1].At.Sub(s[0].At)
}

func looksMains(s []alert.Sample, now time.Time) bool {
	lo, hi, n := math.MaxInt, 0, 0
	for _, x := range s {
		if now.Sub(x.At) > mainsWindow {
			continue
		}
		n++
		lo, hi = min(lo, x.MV), max(hi, x.MV)
	}
	return n >= mainsMinRead && lo >= mainsMinMV && hi-lo <= mainsSwingMV
}

func panelFault(days []Day) (bool, Day) {
	// Only the newest days with a gain count: a fault fixed a fortnight ago
	// shouldn't still be the headline.
	var recent []Day
	for i := len(days) - 1; i >= 0 && len(recent) < 3; i-- {
		if days[i].HasGain {
			recent = append(recent, days[i])
		}
	}
	for i := 0; i+1 < len(recent); i++ {
		if recent[i].PanelSuspect && recent[i+1].PanelSuspect {
			return true, recent[i]
		}
	}
	return false, Day{}
}

// Why there is no break-even. They read on from "Working out how much sun
// it needs ...".
const (
	ReasonFewDays  = "needs at least 5 full days with weather"
	ReasonSameSun  = "needs a mix of sunny and dull days"
	ReasonNoFollow = "can't, because its charge doesn't follow the sunshine"
)

func fit(days []Day) (*Fit, string) {
	var pts [][2]float64
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, d := range days {
		if d.HasNet && d.HasWeather {
			pts = append(pts, [2]float64{d.UsableKWh, float64(d.NetMV)})
			lo, hi = math.Min(lo, d.UsableKWh), math.Max(hi, d.UsableKWh)
		}
	}
	if len(pts) < minFitDays {
		return nil, ReasonFewDays
	}
	if hi-lo < minFitSpreadKWh {
		return nil, ReasonSameSun
	}
	var slopes []float64
	for i := range pts {
		for j := i + 1; j < len(pts); j++ {
			if dx := pts[j][0] - pts[i][0]; math.Abs(dx) > 1e-6 {
				slopes = append(slopes, (pts[j][1]-pts[i][1])/dx)
			}
		}
	}
	m := median(slopes)
	if m <= 0 {
		return nil, ReasonNoFollow
	}
	var ics, res []float64
	for _, p := range pts {
		ics = append(ics, p[1]-m*p[0])
	}
	ic := median(ics)
	for _, p := range pts {
		res = append(res, math.Abs(p[1]-(ic+m*p[0])))
	}
	return &Fit{
		SlopeMVPerKWh: m, InterceptMV: ic, BreakEvenKWh: -ic / m,
		SpreadMV: math.Max(minSpreadMV, median(res)), Days: len(pts),
	}, ""
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// forecast projects the morning lows ahead from the latest known one, using
// the weather forecast and the break-even line.
func forecast(r *Result, s []alert.Sample, in Input) {
	// Nothing heard for a day: whatever is happening now is unknown.
	if r.age > maxStartAge {
		return
	}
	// Start from the newest morning low no more than two days old, or
	// failing that a reading from the last day. Readings often arrive hours
	// apart, so anything tighter leaves good repeaters without a forecast.
	var start time.Time
	var mv float64
	for _, d := range r.Days {
		if d.MorningLowMV > 0 && d.Rise.After(start) {
			start, mv = d.Rise, float64(d.MorningLowMV)
		}
		if d.NextLowMV > 0 && d.NextRise.After(start) {
			start, mv = d.NextRise, float64(d.NextLowMV)
		}
	}
	if start.IsZero() || in.Now.Sub(start) > 48*time.Hour {
		if len(s) == 0 || in.Now.Sub(s[len(s)-1].At) > maxStartAge {
			return
		}
		start, mv = s[len(s)-1].At, float64(s[len(s)-1].MV)
	}
	r.StartAt, r.StartMV = start, mv

	days := sun.Days(start.Add(-time.Minute), start.Add(time.Duration(ForecastDays+2)*24*time.Hour), in.Lat, in.Lon)
	mid := mv
	for i := 0; i+1 < len(days) && len(r.Forecast) < ForecastDays; i++ {
		d := Day{Rise: days[i].Rise, Set: days[i].Set, NextRise: days[i+1].Rise}
		w := dayWeather(in.Weather, d, r.cold)
		if !w.ok {
			break
		}
		sunKWh := w.sunKWh
		net := r.Fit.NetFor(w.usableKWh)
		mid = math.Min(float64(alert.FullMV), mid+net)
		k := float64(len(r.Forecast) + 1)
		spread := r.Fit.SpreadMV * math.Sqrt(k)
		r.Forecast = append(r.Forecast, ForecastDay{
			Rise: d.Rise, Morning: d.NextRise, SunKWh: sunKWh, NetMV: net, MidMV: mid,
			UsableKWh: w.usableKWh, FrozenHours: w.frozen,
			LowMV:  math.Min(float64(alert.FullMV), mid-spread),
			HighMV: math.Min(float64(alert.FullMV), mid+spread),
		})
	}
	for _, lv := range []struct {
		name string
		mv   int
	}{{"critical", in.Levels.CriticalMV}, {"low", in.Levels.LowMV}} {
		if lv.mv <= 0 || mv < float64(lv.mv) {
			continue
		}
		for _, f := range r.Forecast {
			if f.LowMV < float64(lv.mv) {
				r.Crossings = append(r.Crossings, Crossing{Level: lv.name, MV: lv.mv, Day: f.Morning})
				break
			}
		}
	}
}

// ProjPoint is one hour of the projected voltage.
type ProjPoint struct {
	At                   time.Time
	MidMV, LowMV, HighMV float64
}

// Project carries the latest reading forward hour by hour until until,
// using the break-even line: each hour costs a 24th of the line's daily
// loss and gains its slope times that hour's sunshine. That keeps the
// day/night shape a solar battery really has. Nil without a fit, or when
// the latest reading is too old to start from.
func (r Result) Project(hours []weather.Hour, until time.Time) []ProjPoint {
	if r.Fit == nil || r.Mains || r.LatestAt.IsZero() || r.age > maxStartAge {
		return nil
	}
	start, v := r.LatestAt, float64(r.LatestMV)
	out := []ProjPoint{{At: start, MidMV: v, LowMV: v, HighMV: v}}
	last := start
	for _, h := range hours {
		if !h.At.After(start) || h.At.After(until) {
			continue
		}
		// A gap in the weather would make the projection meaningless past it.
		if h.At.Sub(last) > 2*time.Hour {
			break
		}
		frac := h.At.Sub(last).Hours()
		last = h.At
		gain := r.Fit.SlopeMVPerKWh * h.SunWm2 / 1000 * frac
		if r.cold.frozen(h) {
			gain = 0 // the charger won't charge a frozen cell
		}
		v += r.Fit.InterceptMV/24*frac + gain
		v = math.Min(float64(alert.FullMV), v)
		spread := r.Fit.SpreadMV * math.Sqrt(h.At.Sub(start).Hours()/24)
		out = append(out, ProjPoint{At: h.At, MidMV: v,
			LowMV:  v - spread,
			HighMV: math.Min(float64(alert.FullMV), v+spread)})
	}
	if len(out) < 2 {
		return nil
	}
	return out
}
