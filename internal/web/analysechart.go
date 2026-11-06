package web

import (
	"fmt"
	"html/template"
	"math"
	"strings"
	"time"

	"hopreact/internal/alert"
	"hopreact/internal/batteryanalysis"
	"hopreact/internal/corescope"
	"hopreact/internal/sun"
	"hopreact/internal/weather"
)

// The wide chart's geometry, shared with the hover script through the page
// data so a pointer position maps back to a time.
const (
	chartWide  = 960.0
	chartLeft  = 52.0
	chartRight = 946.0
)

type chartInput struct {
	Samples  []alert.Sample
	Temps    []corescope.TempSample
	Hours    []weather.Hour
	Spans    []sun.Span
	From, To time.Time
	Levels   alert.BatteryLevels
	Loc      *time.Location
	// End, when after To, extends the chart into the forecast, and Proj is
	// the projected voltage drawn there.
	End  time.Time
	Proj []batteryanalysis.ProjPoint
}

// axisEnd is where the time axis stops.
func (c chartInput) axisEnd() time.Time {
	if c.End.After(c.To) {
		return c.End
	}
	return c.To
}

// svgBuf builds SVG from numbers and fixed strings only. Nothing a person
// typed goes into it, which is what makes returning it as HTML safe.
type svgBuf struct{ strings.Builder }

func (b *svgBuf) f(format string, a ...any) { fmt.Fprintf(b, format, a...) }

func (b *svgBuf) text(x, y float64, class, anchor, s string) {
	b.f(`<text x="%.1f" y="%.1f" class="%s" text-anchor="%s">%s</text>`, x, y, class, anchor, template.HTMLEscapeString(s))
}

// analysisChart draws voltage over the range with night, twilight and day
// shaded behind it, and below it sunshine, cloud and temperature on the same
// time axis. compact is the phone version: shorter, with sun and cloud
// folded into one strip and no temperature.
func analysisChart(c chartInput, compact bool) template.HTML {
	W, left, right := chartWide, chartLeft, chartRight
	vH := 220.0
	if compact {
		W, left, right, vH = 360, 40, 352, 150
	}
	const top, gap, labels = 10.0, 10.0, 22.0
	type strip struct {
		kind string
		h    float64
	}
	var strips []strip
	hasHours := len(c.Hours) > 0
	switch {
	case compact && hasHours:
		strips = append(strips, strip{"suncloud", 44})
	case !compact:
		if hasHours {
			strips = append(strips, strip{"sun", 58}, strip{"cloud", 30})
		}
		if hasHours || len(c.Temps) > 0 {
			strips = append(strips, strip{"temp", 56})
		}
	}
	H := top + vH + labels
	for _, s := range strips {
		H += gap + s.h
	}
	bottom := H - labels

	t0, t1 := c.From, c.axisEnd()
	x := func(t time.Time) float64 {
		return left + float64(t.Sub(t0))/float64(t1.Sub(t0))*(right-left)
	}
	clampX := func(t time.Time) float64 { return math.Max(left, math.Min(right, x(t))) }

	// Voltage scale.
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, s := range c.Samples {
		lo, hi = math.Min(lo, float64(s.MV)), math.Max(hi, float64(s.MV))
	}
	for _, p := range c.Proj {
		lo, hi = math.Min(lo, p.LowMV), math.Max(hi, p.HighMV)
	}
	if math.IsInf(lo, 1) {
		lo, hi = 3600, 4200
	}
	lo, hi = lo-30, hi+30
	for _, l := range []int{c.Levels.LowMV, c.Levels.CriticalMV} {
		if l > 0 && lo-float64(l) < 150 {
			lo = math.Min(lo, float64(l)-20)
		}
	}
	lo, hi = math.Floor(lo/50)*50, math.Ceil(hi/50)*50
	step := 50.0
	switch {
	case hi-lo > 800:
		step = 200
	case hi-lo > 300:
		step = 100
	}
	y := func(mv float64) float64 { return top + (hi-mv)/(hi-lo)*vH }

	var b svgBuf
	label := fmt.Sprintf("Battery voltage from %s to %s", t0.In(c.Loc).Format("2 Jan"), t1.In(c.Loc).Format("2 Jan"))
	if len(c.Spans) > 0 {
		label += ", shaded for night, twilight and day"
	}
	b.f(`<svg class="ac" viewBox="0 0 %.0f %.0f" role="img" aria-label="%s">`, W, H, template.HTMLEscapeString(label))

	// Phase shading, full height.
	for _, s := range c.Spans {
		x0, x1 := clampX(s.Start), clampX(s.End)
		if x1 > x0 {
			b.f(`<rect x="%.1f" y="%.0f" width="%.2f" height="%.1f" class="ph%d"/>`, x0, top, x1-x0+0.3, bottom-top, int(s.Phase))
		}
	}
	if len(c.Spans) == 0 {
		b.f(`<rect x="%.0f" y="%.0f" width="%.0f" height="%.1f" class="ac-plain"/>`, left, top, right-left, bottom-top)
	}

	// Local midnights, labelled with a stride that keeps labels apart.
	loc := c.Loc
	lt := t0.In(loc)
	pxPerDay := (right - left) / (t1.Sub(t0).Hours() / 24)
	format := "Mon 2"
	minGap := 50.0
	if compact {
		format, minGap = "Mon", 34
	}
	stride := int(math.Max(1, math.Ceil(minGap/pxPerDay)))
	i := 0
	for d := time.Date(lt.Year(), lt.Month(), lt.Day()+1, 0, 0, 0, 0, loc); d.Before(t1); d = d.AddDate(0, 0, 1) {
		xd := x(d)
		b.f(`<line x1="%.1f" x2="%.1f" y1="%.0f" y2="%.1f" class="ac-day"/>`, xd, xd, top, bottom)
		if i%stride == 0 && xd+minGap*0.8 < right+8 {
			b.text(xd+3, H-6, "ac-t", "start", d.Format(format))
		}
		i++
	}

	// Voltage grid and levels.
	for mv := math.Ceil(lo/step) * step; mv <= hi; mv += step {
		b.f(`<line x1="%.0f" x2="%.0f" y1="%.1f" y2="%.1f" class="ac-grid"/>`, left, right, y(mv), y(mv))
		b.text(left-6, y(mv)+4, "ac-t", "end", fmt.Sprintf("%.2f", mv/1000))
	}
	for _, l := range []struct {
		mv          int
		class, name string
	}{{c.Levels.LowMV, "ac-low", "low"}, {c.Levels.CriticalMV, "ac-crit", "critical"}, {alert.FullMV, "ac-full", "full"}} {
		if l.mv <= 0 || float64(l.mv) < lo || float64(l.mv) > hi {
			continue
		}
		b.f(`<line x1="%.0f" x2="%.0f" y1="%.1f" y2="%.1f" class="%s"/>`, left, right, y(float64(l.mv)), y(float64(l.mv)), l.class)
		if !compact {
			b.text(right-4, y(float64(l.mv))-4, l.class+"-t", "end", fmt.Sprintf("%s %.2f V", l.name, float64(l.mv)/1000))
		}
	}

	// Readings, broken where there's a gap of more than 6 hours.
	var pts []string
	flush := func() {
		if len(pts) > 1 {
			b.f(`<polyline class="ac-v" points="%s"/>`, strings.Join(pts, " "))
		}
		pts = pts[:0]
	}
	for j, s := range c.Samples {
		if j > 0 && s.At.Sub(c.Samples[j-1].At) > 6*time.Hour {
			flush()
		}
		pts = append(pts, fmt.Sprintf("%.1f,%.1f", x(s.At), y(float64(s.MV))))
	}
	flush()
	if !compact && len(c.Samples) <= 250 {
		for _, s := range c.Samples {
			b.f(`<circle cx="%.1f" cy="%.1f" r="1.8" class="ac-dot"/>`, x(s.At), y(float64(s.MV)))
		}
	}
	// The forecast: a faint band for the likely range and a dotted line.
	if len(c.Proj) > 1 {
		var band []string
		for _, p := range c.Proj {
			band = append(band, fmt.Sprintf("%.1f,%.1f", x(p.At), y(p.HighMV)))
		}
		for i := len(c.Proj) - 1; i >= 0; i-- {
			band = append(band, fmt.Sprintf("%.1f,%.1f", x(c.Proj[i].At), y(c.Proj[i].LowMV)))
		}
		b.f(`<polygon points="%s" class="ac-band"/>`, strings.Join(band, " "))
		var mid []string
		for _, p := range c.Proj {
			mid = append(mid, fmt.Sprintf("%.1f,%.1f", x(p.At), y(p.MidMV)))
		}
		b.f(`<polyline points="%s" class="ac-proj"/>`, strings.Join(mid, " "))
	}
	if t1.After(c.To) {
		xn := x(c.To)
		b.f(`<line x1="%.1f" x2="%.1f" y1="%.0f" y2="%.1f" class="ac-nowline"/>`, xn, xn, top, bottom)
		b.text(xn+4, top+12, "ac-t ac-nowlabel", "start", "now")
	}
	if n := len(c.Samples); n > 0 {
		s := c.Samples[n-1]
		b.f(`<circle cx="%.1f" cy="%.1f" r="4.5" class="ac-now"/>`, x(s.At), y(float64(s.MV)))
	}
	if !compact {
		b.text(6, top+12, "ac-t", "start", "V")
	}

	// Strips.
	yy := top + vH
	for _, s := range strips {
		yy += gap
		st, h := yy, s.h
		yy += h
		b.f(`<rect x="%.0f" y="%.1f" width="%.0f" height="%.0f" class="ac-strip"/>`, left, st, right-left, h)
		switch s.kind {
		case "sun", "suncloud":
			maxSun := 200.0
			for _, hr := range c.Hours {
				maxSun = math.Max(maxSun, hr.SunWm2)
			}
			maxSun = math.Ceil(maxSun/100) * 100
			var p strings.Builder
			fmt.Fprintf(&p, "M%.1f %.1f", left, st+h)
			for _, hr := range c.Hours {
				mid := hr.At.Add(-30 * time.Minute)
				if mid.Before(t0) || mid.After(t1) {
					continue
				}
				fmt.Fprintf(&p, " L%.1f %.1f", x(mid), st+h-hr.SunWm2/maxSun*h)
			}
			fmt.Fprintf(&p, " L%.1f %.1f Z", right, st+h)
			b.f(`<path d="%s" class="ac-sun"/>`, p.String())
			if s.kind == "suncloud" {
				var q []string
				for _, hr := range c.Hours {
					if hr.At.Before(t0) || hr.At.After(t1) {
						continue
					}
					q = append(q, fmt.Sprintf("%.1f,%.1f", x(hr.At), st+h-hr.CloudPct/100*h))
				}
				if len(q) > 1 {
					b.f(`<polyline points="%s" class="ac-cloudline"/>`, strings.Join(q, " "))
				}
				b.text(left+4, st+11, "ac-t ac-sun-t ac-name", "start", "Sun")
				b.text(left+30, st+11, "ac-t ac-cloud-t ac-name", "start", "Cloud")
			} else {
				b.text(left+4, st+11, "ac-t ac-name", "start", "Sunlight, W/m²")
				b.text(left-6, st+10, "ac-t ac-s", "end", fmt.Sprintf("%.0f", maxSun))
				b.text(left-6, st+h, "ac-t ac-s", "end", "0")
			}
		case "cloud":
			pw := (right - left) / t1.Sub(t0).Hours()
			for _, hr := range c.Hours {
				start := hr.At.Add(-time.Hour)
				if start.Before(t0) || hr.At.After(t1) || hr.CloudPct <= 0 {
					continue
				}
				bh := hr.CloudPct / 100 * h
				b.f(`<rect x="%.1f" y="%.1f" width="%.2f" height="%.1f" class="ac-cloud"/>`, x(start), st+h-bh, math.Max(0.6, pw-0.4), bh)
			}
			b.text(left+4, st+11, "ac-t ac-name", "start", "Cloud")
			b.text(left-6, st+10, "ac-t ac-s", "end", "100%")
			b.text(left-6, st+h, "ac-t ac-s", "end", "0")
		case "temp":
			tlo, thi := math.Inf(1), math.Inf(-1)
			for _, hr := range c.Hours {
				if !hr.At.Before(t0) && !hr.At.After(t1) {
					tlo, thi = math.Min(tlo, hr.TempC), math.Max(thi, hr.TempC)
				}
			}
			for _, t := range c.Temps {
				tlo, thi = math.Min(tlo, t.C), math.Max(thi, t.C)
			}
			if math.IsInf(tlo, 1) {
				continue
			}
			tlo, thi = math.Floor(tlo/5)*5, math.Ceil(thi/5)*5
			if thi-tlo < 10 {
				thi = tlo + 10
			}
			ty := func(v float64) float64 { return st + h - (v-tlo)/(thi-tlo)*h }
			var air []string
			for _, hr := range c.Hours {
				if !hr.At.Before(t0) && !hr.At.After(t1) {
					air = append(air, fmt.Sprintf("%.1f,%.1f", x(hr.At), ty(hr.TempC)))
				}
			}
			if len(air) > 1 {
				b.f(`<polyline points="%s" class="ac-air"/>`, strings.Join(air, " "))
			}
			var dev []string
			for j, t := range c.Temps {
				if j > 0 && t.At.Sub(c.Temps[j-1].At) > 6*time.Hour && len(dev) > 1 {
					b.f(`<polyline points="%s" class="ac-dev"/>`, strings.Join(dev, " "))
					dev = dev[:0]
				}
				dev = append(dev, fmt.Sprintf("%.1f,%.1f", x(t.At), ty(t.C)))
			}
			if len(dev) > 1 {
				b.f(`<polyline points="%s" class="ac-dev"/>`, strings.Join(dev, " "))
			}
			b.text(left+4, st+11, "ac-t ac-name", "start", "Temperature, °C")
			b.text(left-6, st+10, "ac-t ac-s", "end", fmt.Sprintf("%.0f", thi))
			b.text(left-6, st+h, "ac-t ac-s", "end", fmt.Sprintf("%.0f", tlo))
		}
	}
	if !compact {
		b.f(`<line x1="0" x2="0" y1="%.0f" y2="%.1f" class="ac-cross" visibility="hidden"/>`, top, bottom)
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

// forecastChart draws each forecast day's expected net change as a bar
// above or below zero, with the day's sun under it.
func forecastChart(days []batteryanalysis.ForecastDay, loc *time.Location) template.HTML {
	const W, H, left, right, top, bottom = 440.0, 170.0, 40.0, 432.0, 12.0, 136.0
	mid := (top + bottom) / 2
	scale := 20.0
	for _, d := range days {
		scale = math.Max(scale, math.Abs(d.NetMV))
	}
	scale = math.Ceil(scale/10) * 10
	k := (bottom - top) / 2 / scale
	var b svgBuf
	b.f(`<svg class="ac fc" viewBox="0 0 %.0f %.0f" role="img" aria-label="Expected change in battery each day for the next %d days">`, W, H, len(days))
	b.f(`<line x1="%.0f" x2="%.0f" y1="%.1f" y2="%.1f" class="ac-grid"/>`, left, right, mid, mid)
	b.text(left-5, top+8, "ac-t ac-s", "end", fmt.Sprintf("+%.0f", scale))
	b.text(left-5, mid+3, "ac-t ac-s", "end", "0")
	b.text(left-5, bottom, "ac-t ac-s", "end", fmt.Sprintf("−%.0f", scale))
	b.text(2, mid-12, "ac-t ac-s", "start", "mV")
	bw := (right - left) / float64(max(len(days), 1))
	for i, d := range days {
		bx, w := left+float64(i)*bw+bw*0.18, bw*0.64
		v := d.NetMV * k
		class := "fc-up"
		y0 := mid - v
		if v < 0 {
			class, y0 = "fc-down", mid
		}
		b.f(`<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" rx="2" class="%s"/>`, bx, y0, w, math.Max(1, math.Abs(v)), class)
		b.text(bx+w/2, bottom+14, "ac-t", "middle", d.Rise.In(loc).Format("Mon"))
		b.text(bx+w/2, bottom+27, "ac-t ac-s ac-sun-t", "middle", fmt.Sprintf("%.1f", d.SunKWh))
	}
	b.text(left-5, bottom+27, "ac-t ac-s ac-sun-t", "end", "sun")
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}
