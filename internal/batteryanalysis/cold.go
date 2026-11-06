package batteryanalysis

import (
	"sort"
	"time"

	"hopreact/internal/corescope"
	"hopreact/internal/weather"
)

// coldModel estimates the battery's temperature for any hour: the
// repeater's own reading when there is one near it, otherwise the air
// temperature plus how much warmer than the air the repeater runs in
// daylight. An enclosure in the sun runs well above the air, so freezing air
// doesn't always mean a freezing battery.
type coldModel struct {
	temps     []corescope.TempSample // sorted by time
	offset    float64
	hasOffset bool
}

// nearReading is how close the repeater's own reading must be to an hour to
// stand for it.
const nearReading = 90 * time.Minute

func newColdModel(temps []corescope.TempSample, hours []weather.Hour) coldModel {
	c := coldModel{temps: append([]corescope.TempSample(nil), temps...)}
	sort.Slice(c.temps, func(i, j int) bool { return c.temps[i].At.Before(c.temps[j].At) })
	// Daylight pairs of the repeater's reading and the air that hour.
	var diffs []float64
	for _, t := range c.temps {
		for _, h := range hours {
			// h covers the hour ending at h.At.
			if h.At.After(t.At) && !h.At.After(t.At.Add(time.Hour)) {
				if h.SunWm2 > 0 {
					diffs = append(diffs, t.C-h.TempC)
				}
				break
			}
		}
	}
	if len(diffs) >= minOffsetPairs {
		c.offset, c.hasOffset = median(diffs), true
	}
	return c
}

// own is the repeater's reading nearest the middle of the hour ending at,
// if one is close enough.
func (c coldModel) own(at time.Time) (float64, bool) {
	mid := at.Add(-30 * time.Minute)
	i := sort.Search(len(c.temps), func(i int) bool { return !c.temps[i].At.Before(mid) })
	best, gap := 0.0, nearReading+1
	for _, j := range []int{i - 1, i} {
		if j < 0 || j >= len(c.temps) {
			continue
		}
		d := c.temps[j].At.Sub(mid)
		if d < 0 {
			d = -d
		}
		if d < gap {
			best, gap = c.temps[j].C, d
		}
	}
	return best, gap <= nearReading
}

// batteryTemp is the estimated battery temperature for an hour, and whether
// it is an estimate rather than the repeater's own reading.
func (c coldModel) batteryTemp(h weather.Hour) (float64, bool) {
	if t, ok := c.own(h.At); ok {
		return t, false
	}
	if c.hasOffset && h.SunWm2 > 0 {
		return h.TempC + c.offset, true
	}
	return h.TempC, true
}

// frozen says the sun shone that hour but the battery was too cold to take
// a charge.
func (c coldModel) frozen(h weather.Hour) bool {
	if h.SunWm2 <= 0 {
		return false
	}
	t, _ := c.batteryTemp(h)
	return t < NoChargeBelowC
}

// ColdHour is one hour's estimated battery temperature.
type ColdHour struct {
	At        time.Time // end of the hour
	TempC     float64
	Estimated bool
	// Frozen is a sunny hour too cold to charge in.
	Frozen bool
}

// ColdHours estimates the battery's temperature for each weather hour
// between from and to.
func (r Result) ColdHours(hours []weather.Hour, from, to time.Time) []ColdHour {
	var out []ColdHour
	for _, h := range hours {
		if h.At.Before(from) || h.At.After(to) {
			continue
		}
		t, est := r.cold.batteryTemp(h)
		out = append(out, ColdHour{At: h.At, TempC: t, Estimated: est, Frozen: r.cold.frozen(h)})
	}
	return out
}
