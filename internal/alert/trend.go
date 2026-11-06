package alert

import (
	"hopreact/internal/corescope"
	"math"
	"sort"
	"time"
)

// TrendKind is which way a battery is going.
type TrendKind string

const (
	// TrendInsufficient means fewer than three usable days of readings.
	TrendInsufficient TrendKind = "insufficient"
	// TrendJumpy means the readings swing too much to trust a line through
	// them, which is what a noisy board or a charger reading looks like.
	TrendJumpy       TrendKind = "jumpy"
	TrendSteady      TrendKind = "steady"
	TrendDraining    TrendKind = "draining"
	TrendCharging    TrendKind = "charging"
	TrendChargingNow TrendKind = "charging_now"
	TrendFull        TrendKind = "full"
)

// Sample is one battery reading.
type Sample = corescope.BatterySample

// Trend thresholds, worked out from a week of real readings across 24
// ScotMesh repeaters (Nov 2026). Healthy solar repeaters drift by less than
// ±10 mV a day once the day/night swing is taken out, read in 10 mV steps and
// rarely move more than 20 mV between hourly readings.
const (
	// trendWindow is how far back the trend looks.
	trendWindow = 7 * 24 * time.Hour
	// minNights is how many usable days a direction needs.
	minNights = 3
	// minReadingsPerDay is how many readings a day needs before its lowest
	// one can stand for that night.
	minReadingsPerDay = 6
	// directionMVPerDay is the slope both ends of the likely range must pass
	// before a battery counts as draining or charging.
	directionMVPerDay = 10
	// jumpMV is a step between two readings too big for a battery to make on
	// its own in an hour or two: a battery swap, a charger plugged in, or a
	// bad reading.
	jumpMV = 150
	// maxJumps is how many such jumps a week can have before the readings are
	// called too jumpy to estimate.
	maxJumps = 3
	// jumpyMedianStepMV is the typical step between readings above which the
	// readings are too noisy for a line to mean anything.
	jumpyMedianStepMV = 40
	// chargingNowWindow and chargingNowMVPerHour spot a battery being charged
	// right now. A sunny morning lifts a solar cell by about 20-30 mV an hour,
	// a USB charger by well over 50.
	chargingNowWindow    = 3 * time.Hour
	chargingNowMVPerHour = 40
	// FullMV is a fully charged single lithium-ion cell, and fullDailyHighMV
	// the highest reading of the last day that counts as reaching it.
	FullMV          = 4200
	fullDailyHighMV = 4180
	nearlyFullMV    = 4150
)

// Trend is a battery's direction and rate.
type Trend struct {
	Kind TrendKind
	// SlopeMVPerDay is the median day-to-day change in the nightly lows, and
	// SlopeLow/SlopeHigh the 25th and 75th percentiles, so SlopeLow is the
	// steeper fall (or slower rise).
	SlopeMVPerDay float64
	SlopeLow      float64
	SlopeHigh     float64
	// RateMVPerHour is set for TrendChargingNow.
	RateMVPerHour float64
	// Nights is how many days the slope came from.
	Nights int
	// LastLowMV is the most recent night's lowest reading, the starting point
	// for "how long until" while draining. LatestMV is the newest reading and
	// DailyHighMV the highest of the last 24 hours.
	LastLowMV   int
	LatestMV    int
	DailyHighMV int
	// NearlyFull is set while charging now above 4.15 V, where chargers slow
	// down and a straight line would promise too much.
	NearlyFull bool
}

// AnalyseTrend works out which way a battery is going from its readings.
//
// The day/night swing is the thing to get past: a solar repeater falls 20-100
// mV overnight and recovers in daylight, so a line through raw readings mostly
// measures the time of day. Each day's lowest reading, just before dawn after
// a night without sun, is the comparable figure, so the slope is fitted to
// those. loc decides where one day ends.
func AnalyseTrend(samples []Sample, now time.Time, loc *time.Location) Trend {
	if loc == nil {
		loc = time.UTC
	}
	var s []Sample
	for _, x := range samples {
		if x.MV > 0 && !x.At.After(now) && now.Sub(x.At) <= trendWindow {
			s = append(s, x)
		}
	}
	sort.Slice(s, func(i, j int) bool { return s[i].At.Before(s[j].At) })
	if len(s) == 0 {
		return Trend{Kind: TrendInsufficient}
	}
	t := Trend{LatestMV: s[len(s)-1].MV}
	for _, x := range s {
		if now.Sub(x.At) <= 24*time.Hour && x.MV > t.DailyHighMV {
			t.DailyHighMV = x.MV
		}
	}

	// Jumps: count them, and restart the trend after the last one that held,
	// since a swapped battery's history says nothing about the new one.
	jumps := 0
	start := 0
	steps := make([]int, 0, len(s))
	for i := 1; i < len(s); i++ {
		d := s[i].MV - s[i-1].MV
		steps = append(steps, absInt(d))
		if absInt(d) <= jumpMV {
			continue
		}
		jumps++
		held := i == len(s)-1 || absInt(s[i+1].MV-s[i].MV) <= jumpMV/2
		if held {
			start = i
		}
	}
	if jumps > maxJumps || (len(steps) >= minReadingsPerDay && medianInt(steps) > jumpyMedianStepMV) {
		t.Kind = TrendJumpy
		return t
	}
	s = s[start:]

	if rate, ok := chargingRate(s, now); ok {
		t.Kind, t.RateMVPerHour = TrendChargingNow, rate
		t.NearlyFull = t.LatestMV >= nearlyFullMV
		return t
	}

	// Nightly lows, one per local day with enough readings to trust.
	type day struct {
		key string
		at  time.Time
		low int
		n   int
	}
	var days []*day
	byKey := map[string]*day{}
	for _, x := range s {
		lt := x.At.In(loc)
		k := lt.Format("2006-01-02")
		d := byKey[k]
		if d == nil {
			mid := time.Date(lt.Year(), lt.Month(), lt.Day(), 0, 0, 0, 0, loc)
			d = &day{key: k, at: mid, low: x.MV}
			byKey[k] = d
			days = append(days, d)
		}
		d.n++
		if x.MV < d.low {
			d.low = x.MV
		}
	}
	var pts [][2]float64
	for _, d := range days {
		if d.n >= minReadingsPerDay {
			pts = append(pts, [2]float64{d.at.Sub(days[0].at).Hours() / 24, float64(d.low)})
			t.LastLowMV = d.low
		}
	}
	t.Nights = len(pts)
	if len(pts) < minNights {
		if t.DailyHighMV >= fullDailyHighMV {
			t.Kind = TrendFull
			return t
		}
		t.Kind = TrendInsufficient
		return t
	}

	t.SlopeMVPerDay, t.SlopeLow, t.SlopeHigh = theilSen(pts)
	switch {
	case t.SlopeHigh <= -directionMVPerDay:
		t.Kind = TrendDraining
	case t.DailyHighMV >= fullDailyHighMV:
		t.Kind = TrendFull
	case t.SlopeLow >= directionMVPerDay:
		t.Kind = TrendCharging
	default:
		t.Kind = TrendSteady
	}
	return t
}

// chargingRate spots a battery being charged right now: a steady climb over
// the last few hours, fast enough that it isn't just morning sun.
func chargingRate(s []Sample, now time.Time) (float64, bool) {
	var w []Sample
	for _, x := range s {
		if now.Sub(x.At) <= chargingNowWindow {
			w = append(w, x)
		}
	}
	if len(w) < 3 || w[len(w)-1].At.Sub(w[0].At) < 2*time.Hour {
		return 0, false
	}
	for i := 1; i < len(w); i++ {
		if w[i].MV < w[i-1].MV-10 {
			return 0, false // fell on the way: not a charger
		}
	}
	rate := float64(w[len(w)-1].MV-w[0].MV) / w[len(w)-1].At.Sub(w[0].At).Hours()
	return rate, rate >= chargingNowMVPerHour
}

// theilSen fits a line by the median of the slopes between every pair of
// points, so one odd day cannot swing it, and returns the median with the 25th
// and 75th percentile slopes as a likely range.
func theilSen(pts [][2]float64) (median, low, high float64) {
	var sl []float64
	for i := range pts {
		for j := i + 1; j < len(pts); j++ {
			dx := pts[j][0] - pts[i][0]
			if dx > 0 {
				sl = append(sl, (pts[j][1]-pts[i][1])/dx)
			}
		}
	}
	if len(sl) == 0 {
		return 0, 0, 0
	}
	sort.Float64s(sl)
	q := func(p float64) float64 {
		// Linear interpolation between closest ranks.
		pos := p * float64(len(sl)-1)
		lo := int(math.Floor(pos))
		hi := int(math.Ceil(pos))
		return sl[lo] + (sl[hi]-sl[lo])*(pos-float64(lo))
	}
	return q(0.5), q(0.25), q(0.75)
}

// Estimate is how long until a battery reaches one level at its current rate.
type Estimate struct {
	MV int
	// Soonest and Latest bound the likely time. Both zero with Reached set
	// means it is already there.
	Soonest, Latest time.Duration
	Reached         bool
	// Beyond means more than MaxEstimate away, or not heading there at all.
	Beyond bool
}

// MaxEstimate is the furthest ahead an estimate is given. Past two months a
// straight line through a week of readings says nothing useful.
const MaxEstimate = 60 * 24 * time.Hour

// TimeTo estimates when a draining battery reaches mv, from its last nightly
// low. Lithium cells fall faster below about 3.6 V, so near the bottom the
// line is optimistic and Latest is best read as "at most".
func (t Trend) TimeTo(mv int) Estimate {
	e := Estimate{MV: mv}
	if t.Kind != TrendDraining || t.LastLowMV == 0 {
		e.Beyond = true
		return e
	}
	if t.LastLowMV <= mv {
		e.Reached = true
		return e
	}
	gap := float64(t.LastLowMV - mv)
	e.Soonest = days(gap / -t.SlopeLow)
	if t.SlopeHigh < 0 {
		e.Latest = days(gap / -t.SlopeHigh)
	} else {
		e.Latest = MaxEstimate + time.Hour
	}
	if e.Soonest > MaxEstimate {
		e.Beyond = true
	}
	return e
}

// TimeToFull estimates when a charging battery reaches a full 4.20 V.
func (t Trend) TimeToFull() Estimate {
	e := Estimate{MV: FullMV}
	switch t.Kind {
	case TrendFull:
		e.Reached = true
	case TrendChargingNow:
		if t.LatestMV >= FullMV {
			e.Reached = true
			break
		}
		h := float64(FullMV-t.LatestMV) / t.RateMVPerHour
		e.Soonest = time.Duration(h * float64(time.Hour))
		e.Latest = e.Soonest
	case TrendCharging:
		from := t.DailyHighMV
		if from == 0 {
			from = t.LatestMV
		}
		if from >= FullMV {
			e.Reached = true
			break
		}
		gap := float64(FullMV - from)
		e.Soonest = days(gap / t.SlopeHigh)
		e.Latest = days(gap / t.SlopeLow)
		if e.Soonest > MaxEstimate {
			e.Beyond = true
		}
	default:
		e.Beyond = true
	}
	return e
}

func days(d float64) time.Duration { return time.Duration(d * 24 * float64(time.Hour)) }

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func medianInt(v []int) int {
	c := append([]int(nil), v...)
	sort.Ints(c)
	return c[len(c)/2]
}
