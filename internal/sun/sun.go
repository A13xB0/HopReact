// Package sun works out where the sun is for a place and time: its height
// above the horizon, sunrise and sunset, and the twilight phases between.
//
// It uses NOAA's solar position equations, which are good to about a minute
// for sunrise and sunset at UK latitudes. Nothing here touches the network.
package sun

import (
	"math"
	"sort"
	"time"
)

// Phase is how dark it is, from the sun's height.
type Phase int

const (
	Night Phase = iota
	Astronomical
	Nautical
	Civil
	Day
)

// Sun heights in degrees that separate the phases. Sunrise and sunset are
// when the sun's upper edge meets the horizon, allowing for refraction.
const (
	HorizonDeg      = -0.833
	CivilDeg        = -6.0
	NauticalDeg     = -12.0
	AstronomicalDeg = -18.0
)

// Name is the phase as people say it.
func (p Phase) Name() string {
	switch p {
	case Day:
		return "Day"
	case Civil:
		return "Civil twilight"
	case Nautical:
		return "Nautical twilight"
	case Astronomical:
		return "Astronomical twilight"
	}
	return "Night"
}

// PhaseOf grades a sun height.
func PhaseOf(elevationDeg float64) Phase {
	switch {
	case elevationDeg >= HorizonDeg:
		return Day
	case elevationDeg >= CivilDeg:
		return Civil
	case elevationDeg >= NauticalDeg:
		return Nautical
	case elevationDeg >= AstronomicalDeg:
		return Astronomical
	}
	return Night
}

const rad = math.Pi / 180

// Elevation is the sun's height above the horizon in degrees, without
// refraction, at t for a place at lat, lon (degrees, east positive).
func Elevation(t time.Time, lat, lon float64) float64 {
	t = t.UTC()
	jd := float64(t.UnixNano())/float64(24*time.Hour) + 2440587.5
	T := (jd - 2451545.0) / 36525

	l0 := math.Mod(280.46646+T*(36000.76983+T*0.0003032), 360)
	m := 357.52911 + T*(35999.05029-0.0001537*T)
	e := 0.016708634 - T*(0.000042037+0.0000001267*T)
	c := math.Sin(m*rad)*(1.914602-T*(0.004817+0.000014*T)) +
		math.Sin(2*m*rad)*(0.019993-0.000101*T) +
		math.Sin(3*m*rad)*0.000289
	omega := 125.04 - 1934.136*T
	lambda := l0 + c - 0.00569 - 0.00478*math.Sin(omega*rad)
	eps0 := 23 + (26+(21.448-T*(46.815+T*(0.00059-T*0.001813)))/60)/60
	eps := eps0 + 0.00256*math.Cos(omega*rad)
	decl := math.Asin(math.Sin(eps*rad) * math.Sin(lambda*rad))

	y := math.Pow(math.Tan(eps*rad/2), 2)
	eot := 4 / rad * (y*math.Sin(2*l0*rad) - 2*e*math.Sin(m*rad) +
		4*e*y*math.Sin(m*rad)*math.Cos(2*l0*rad) -
		0.5*y*y*math.Sin(4*l0*rad) - 1.25*e*e*math.Sin(2*m*rad))

	minutes := float64(t.Hour()*60+t.Minute()) + float64(t.Second())/60 + float64(t.Nanosecond())/6e10
	trueSolar := math.Mod(minutes+eot+4*lon, 1440)
	ha := trueSolar/4 - 180
	cosZ := math.Sin(lat*rad)*math.Sin(decl) + math.Cos(lat*rad)*math.Cos(decl)*math.Cos(ha*rad)
	cosZ = math.Max(-1, math.Min(1, cosZ))
	return 90 - math.Acos(cosZ)/rad
}

// Crossing is a moment the sun passes a height, going up or down.
type Crossing struct {
	At time.Time
	Up bool
}

// scanStep is how often the sun's height is sampled before a crossing is
// narrowed down. The sun never crosses a height twice in 10 minutes.
const scanStep = 10 * time.Minute

// Crossings lists every time between from and to that the sun passes
// heightDeg, to within a second.
func Crossings(from, to time.Time, lat, lon, heightDeg float64) []Crossing {
	var out []Crossing
	prevT := from
	prev := Elevation(from, lat, lon) - heightDeg
	for t := from.Add(scanStep); ; t = t.Add(scanStep) {
		if t.After(to) {
			t = to
		}
		cur := Elevation(t, lat, lon) - heightDeg
		if (prev < 0) != (cur < 0) {
			lo, hi := prevT, t
			for hi.Sub(lo) > time.Second {
				mid := lo.Add(hi.Sub(lo) / 2)
				if (Elevation(mid, lat, lon)-heightDeg < 0) == (prev < 0) {
					lo = mid
				} else {
					hi = mid
				}
			}
			out = append(out, Crossing{At: hi.Truncate(time.Second), Up: cur >= 0})
		}
		if !t.Before(to) {
			break
		}
		prevT, prev = t, cur
	}
	return out
}

// Span is one stretch of a single phase.
type Span struct {
	Start, End time.Time
	Phase      Phase
}

// Spans splits from..to into its phases, in order, with no gaps.
func Spans(from, to time.Time, lat, lon float64) []Span {
	if !to.After(from) {
		return nil
	}
	bounds := []time.Time{from, to}
	for _, h := range []float64{HorizonDeg, CivilDeg, NauticalDeg, AstronomicalDeg} {
		for _, c := range Crossings(from, to, lat, lon, h) {
			bounds = append(bounds, c.At)
		}
	}
	sort.Slice(bounds, func(i, j int) bool { return bounds[i].Before(bounds[j]) })
	var out []Span
	for i := 1; i < len(bounds); i++ {
		a, b := bounds[i-1], bounds[i]
		if !b.After(a) {
			continue
		}
		p := PhaseOf(Elevation(a.Add(b.Sub(a)/2), lat, lon))
		if n := len(out); n > 0 && out[n-1].Phase == p {
			out[n-1].End = b
			continue
		}
		out = append(out, Span{Start: a, End: b, Phase: p})
	}
	return out
}

// Daylight is one sunrise and the sunset after it.
type Daylight struct {
	Rise, Set time.Time
}

// Length is how long the sun is up.
func (d Daylight) Length() time.Duration { return d.Set.Sub(d.Rise) }

// Days lists every sunrise between from and to that has its sunset before to.
func Days(from, to time.Time, lat, lon float64) []Daylight {
	var out []Daylight
	var rise time.Time
	for _, c := range Crossings(from, to, lat, lon, HorizonDeg) {
		switch {
		case c.Up:
			rise = c.At
		case !rise.IsZero():
			out = append(out, Daylight{Rise: rise, Set: c.At})
			rise = time.Time{}
		}
	}
	return out
}

// On is the daylight of the local calendar day containing t, if the sun
// rises and sets that day.
func On(t time.Time, loc *time.Location, lat, lon float64) (Daylight, bool) {
	if loc == nil {
		loc = time.UTC
	}
	l := t.In(loc)
	start := time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, loc)
	d := Days(start, start.AddDate(0, 0, 1), lat, lon)
	if len(d) == 0 {
		return Daylight{}, false
	}
	return d[0], true
}
