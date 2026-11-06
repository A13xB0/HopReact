package notify

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"strings"
	"sync"
	"time"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
	"golang.org/x/image/vector"

	"hopreact/internal/alert"
	"hopreact/internal/batteryview"
)

// Chart size. Twice the size it shows at in Discord, so it stays sharp on
// high-density screens.
const (
	chartW = 1040
	chartH = 440
)

// shutOffMV is where most repeaters shut themselves off.
const shutOffMV = batteryview.ShutOffMV

// Colours, matched to Discord's dark theme so the chart sits in the message.
var (
	cBG       = color.RGBA{0x1e, 0x1f, 0x22, 0xff}
	cGrid     = color.RGBA{0x2b, 0x2d, 0x31, 0xff}
	cAxis     = color.RGBA{0x4e, 0x50, 0x58, 0xff}
	cMuted    = color.RGBA{0x94, 0x9b, 0xa4, 0xff}
	cText     = color.RGBA{0xdb, 0xde, 0xe1, 0xff}
	cLine     = color.RGBA{0x5a, 0xa9, 0xff, 0xff}
	cBand     = color.RGBA{0x5a, 0xa9, 0xff, 0x29}
	cLow      = color.RGBA{0xfa, 0xcc, 0x15, 0xff}
	cCritical = color.RGBA{0xf8, 0x71, 0x71, 0xff}
	cWhite    = color.RGBA{0xff, 0xff, 0xff, 0xff}
)

var (
	fontsOnce sync.Once
	fontsErr  error
	faceSmall font.Face
	faceBold  font.Face
)

func loadFonts() error {
	fontsOnce.Do(func() {
		reg, err := opentype.Parse(goregular.TTF)
		if err != nil {
			fontsErr = err
			return
		}
		bold, err := opentype.Parse(gobold.TTF)
		if err != nil {
			fontsErr = err
			return
		}
		if faceSmall, err = opentype.NewFace(reg, &opentype.FaceOptions{Size: 20, DPI: 72, Hinting: font.HintingFull}); err != nil {
			fontsErr = err
			return
		}
		faceBold, fontsErr = opentype.NewFace(bold, &opentype.FaceOptions{Size: 22, DPI: 72, Hinting: font.HintingFull})
	})
	return fontsErr
}

// ChartInput is what one battery chart shows.
type ChartInput struct {
	Name    string
	Samples []alert.Sample
	Now     time.Time
	Trend   alert.Trend
	Levels  alert.BatteryLevels
	Loc     *time.Location
}

// BatteryChart draws a week of readings with the levels, and, when the
// battery is clearly draining or charging, the trend carried forward with its
// likely range. It returns a PNG.
func BatteryChart(in ChartInput) ([]byte, error) {
	if err := loadFonts(); err != nil {
		return nil, err
	}
	if len(in.Samples) == 0 {
		return nil, fmt.Errorf("chart: no readings")
	}
	loc := in.Loc
	if loc == nil {
		loc = time.UTC
	}

	// Plot area.
	const left, right, top, bottom = 72.0, 1016.0, 48.0, 380.0

	forecast := in.Trend.Kind == alert.TrendDraining || in.Trend.Kind == alert.TrendCharging
	t0 := in.Now.Add(-7 * 24 * time.Hour)
	t1 := in.Now.Add(12 * time.Hour)
	if forecast {
		t1 = in.Now.Add(8 * 24 * time.Hour)
	}

	// Voltage range: the readings, the levels that matter, and the forecast.
	lo, hi := math.Inf(1), math.Inf(-1)
	widen := func(mv float64) { lo, hi = math.Min(lo, mv), math.Max(hi, mv) }
	for _, s := range in.Samples {
		if !s.At.Before(t0) {
			widen(float64(s.MV))
		}
	}
	if math.IsInf(lo, 1) {
		return nil, fmt.Errorf("chart: no readings in the last week")
	}
	// Only stretch up to the overcharge line when the readings come near it;
	// otherwise a draining battery's fall would be squeezed into a thin band.
	if in.Levels.OverchargeMV > 0 && hi >= float64(in.Levels.OverchargeMV)-300 {
		widen(float64(in.Levels.OverchargeMV))
	}
	widen(float64(in.Levels.LowMV))
	widen(float64(in.Levels.CriticalMV))
	// The forecast starts where the estimates in the message start: the last
	// night's low while draining, the last day's high while charging.
	start := float64(in.Trend.LastLowMV)
	if in.Trend.Kind == alert.TrendCharging && in.Trend.DailyHighMV > 0 {
		start = float64(in.Trend.DailyHighMV)
	}
	if start == 0 {
		start = float64(in.Trend.LatestMV)
	}
	days := t1.Sub(in.Now).Hours() / 24
	if in.Trend.Kind == alert.TrendDraining && batteryview.ShutOffDistinct(in.Levels) {
		widen(float64(shutOffMV))
		widen(math.Max(start+in.Trend.SlopeLow*days, shutOffMV-100))
	}
	if in.Trend.Kind == alert.TrendCharging {
		widen(math.Min(start+in.Trend.SlopeHigh*days, alert.FullMV+50))
	}
	lo = math.Floor((lo-50)/100) * 100
	hi = math.Ceil((hi+50)/100) * 100

	xAt := func(t time.Time) float64 {
		return left + float64(t.Sub(t0))/float64(t1.Sub(t0))*(right-left)
	}
	yAt := func(mv float64) float64 {
		mv = math.Max(lo, math.Min(hi, mv))
		return bottom - (mv-lo)/(hi-lo)*(bottom-top)
	}

	img := image.NewRGBA(image.Rect(0, 0, chartW, chartH))
	draw.Draw(img, img.Bounds(), &image.Uniform{cBG}, image.Point{}, draw.Src)
	c := &canvas{img: img}

	// Grid every 100 or 200 mV, labelled in volts.
	step := 100.0
	if hi-lo > 1000 {
		step = 200
	}
	for mv := lo; mv <= hi+1; mv += step {
		y := yAt(mv)
		c.line(left, y, right, y, 2, cGrid, nil)
		c.text(left-10, y+7, fmt.Sprintf("%.1f", mv/1000), faceSmall, cMuted, alignRight)
	}
	// Day ticks at local midnights, named by weekday.
	for d := time.Date(t0.In(loc).Year(), t0.In(loc).Month(), t0.In(loc).Day()+1, 0, 0, 0, 0, loc); d.Before(t1); d = d.AddDate(0, 0, 1) {
		x := xAt(d)
		c.line(x, bottom, x, bottom+8, 2, cAxis, nil)
		label := d.Format("Mon")
		if x > right-30 {
			continue
		}
		c.text(x+30, bottom+30, label, faceSmall, cMuted, alignCentre)
	}
	nowX := xAt(in.Now)
	c.line(nowX, top, nowX, bottom, 2, cAxis, nil)
	c.text(nowX, bottom+56, "now", faceSmall, cText, alignCentre)

	// Levels.
	type level struct {
		mv    int
		col   color.RGBA
		label string
	}
	levels := []level{
		{in.Levels.LowMV, cLow, fmt.Sprintf("low %.2f", float64(in.Levels.LowMV)/1000)},
		{in.Levels.CriticalMV, cCritical, fmt.Sprintf("critical %.2f", float64(in.Levels.CriticalMV)/1000)},
	}
	if in.Trend.Kind == alert.TrendDraining && float64(shutOffMV) >= lo && batteryview.ShutOffDistinct(in.Levels) {
		levels = append(levels, level{shutOffMV, cMuted, "shuts off ~3.30"})
	}
	if in.Trend.Kind == alert.TrendCharging {
		levels = append(levels, level{alert.FullMV, cMuted, "full 4.20"})
	}
	if in.Levels.OverchargeMV > 0 && float64(in.Levels.OverchargeMV) <= hi {
		levels = append(levels, level{in.Levels.OverchargeMV, cCritical, fmt.Sprintf("overcharge %.2f", float64(in.Levels.OverchargeMV)/1000)})
	}
	for _, l := range levels {
		y := yAt(float64(l.mv))
		c.line(left, y, right, y, 2.4, l.col, []float64{10, 8})
		// Labelled at the left, where the readings start, so the forecast's
		// marks on the right never sit on top of them.
		c.text(left+8, y-8, l.label, faceSmall, l.col, alignLeft)
	}

	// Forecast: a shaded likely range and a dashed median line.
	if forecast {
		end := t1
		x0, y0 := nowX, yAt(start)
		ptAt := func(slope float64) (float64, float64) {
			return xAt(end), yAt(start + slope*days)
		}
		ax, ay := ptAt(in.Trend.SlopeLow)
		bx, by := ptAt(in.Trend.SlopeHigh)
		c.fill([][2]float64{{x0, y0}, {ax, ay}, {bx, by}}, cBand)
		mx, my := ptAt(in.Trend.SlopeMVPerDay)
		c.line(x0, y0, mx, my, 3, cLine, []float64{12, 8})

		// Mark where the median meets each level ahead: the levels below
		// while draining, full while charging.
		for _, l := range levels {
			var d float64
			switch {
			case in.Trend.Kind == alert.TrendDraining && l.mv < int(start) && l.mv != in.Levels.OverchargeMV:
				d = (start - float64(l.mv)) / -in.Trend.SlopeMVPerDay
			case in.Trend.Kind == alert.TrendCharging && l.mv == alert.FullMV && float64(l.mv) > start:
				d = (float64(l.mv) - start) / in.Trend.SlopeMVPerDay
			default:
				continue
			}
			at := in.Now.Add(time.Duration(d * 24 * float64(time.Hour)))
			if d <= 0 || at.After(t1) {
				continue
			}
			x, y := xAt(at), yAt(float64(l.mv))
			c.dot(x, y, 9, cBG)
			c.dot(x, y, 7, l.col)
			label := "~" + at.In(loc).Format("Mon")
			align, lx := alignLeft, x+12
			if x > right-120 {
				align, lx = alignRight, x-12
			}
			ly := y + 30
			if in.Trend.Kind == alert.TrendCharging {
				ly = y - 14
			}
			c.text(lx, ly, label, faceBold, l.col, align)
		}
	}

	// Readings.
	var pts [][2]float64
	for _, s := range in.Samples {
		if s.At.Before(t0) || s.At.After(in.Now) {
			continue
		}
		pts = append(pts, [2]float64{xAt(s.At), yAt(float64(s.MV))})
	}
	for i := 1; i < len(pts); i++ {
		c.line(pts[i-1][0], pts[i-1][1], pts[i][0], pts[i][1], 4, cLine, nil)
	}
	if n := len(pts); n > 0 {
		c.dot(pts[n-1][0], pts[n-1][1], 8, cWhite)
	}

	title := drawable(in.Name, faceBold) + " · last 7 days"
	if forecast {
		title += " and the trend ahead"
	}
	c.text(left+8, top-14, title, faceBold, cText, alignLeft)

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// drawable drops characters the chart font has no glyph for, such as the
// emoji people put in repeater names, which would otherwise draw as boxes.
func drawable(s string, face font.Face) string {
	var b strings.Builder
	for _, r := range s {
		if _, ok := face.GlyphAdvance(r); ok && r != '\uFE0F' {
			b.WriteRune(r)
		}
	}
	out := strings.Join(strings.Fields(b.String()), " ")
	if out == "" {
		return "Repeater"
	}
	return out
}

type align int

const (
	alignLeft align = iota
	alignCentre
	alignRight
)

// canvas draws antialiased shapes onto an image.
type canvas struct {
	img *image.RGBA
}

func (c *canvas) fill(poly [][2]float64, col color.Color) {
	if len(poly) < 3 {
		return
	}
	// Rasterise only the shape's own box. A full-size rasterizer per line
	// segment made one chart take seconds.
	minX, minY, maxX, maxY := poly[0][0], poly[0][1], poly[0][0], poly[0][1]
	for _, p := range poly[1:] {
		minX, minY = math.Min(minX, p[0]), math.Min(minY, p[1])
		maxX, maxY = math.Max(maxX, p[0]), math.Max(maxY, p[1])
	}
	box := image.Rect(int(math.Floor(minX))-1, int(math.Floor(minY))-1,
		int(math.Ceil(maxX))+1, int(math.Ceil(maxY))+1).Intersect(c.img.Bounds())
	if box.Empty() {
		return
	}
	r := vector.NewRasterizer(box.Dx(), box.Dy())
	r.DrawOp = draw.Over
	ox, oy := float32(box.Min.X), float32(box.Min.Y)
	r.MoveTo(float32(poly[0][0])-ox, float32(poly[0][1])-oy)
	for _, p := range poly[1:] {
		r.LineTo(float32(p[0])-ox, float32(p[1])-oy)
	}
	r.ClosePath()
	r.Draw(c.img, box, &image.Uniform{col}, image.Point{})
}

// line strokes a segment as a thin quad, optionally dashed (on, off, ...).
func (c *canvas) line(x0, y0, x1, y1, width float64, col color.Color, dash []float64) {
	dx, dy := x1-x0, y1-y0
	length := math.Hypot(dx, dy)
	if length == 0 {
		return
	}
	ux, uy := dx/length, dy/length
	nx, ny := -uy*width/2, ux*width/2
	seg := func(a, b float64) {
		ax, ay := x0+ux*a, y0+uy*a
		bx, by := x0+ux*b, y0+uy*b
		c.fill([][2]float64{{ax + nx, ay + ny}, {bx + nx, by + ny}, {bx - nx, by - ny}, {ax - nx, ay - ny}}, col)
	}
	if len(dash) < 2 {
		seg(0, length)
		if width >= 3 {
			// Round the ends so joined segments meet without notches.
			c.dot(x0, y0, width/2, col)
			c.dot(x1, y1, width/2, col)
		}
		return
	}
	for pos, i := 0.0, 0; pos < length; i++ {
		d := dash[i%len(dash)]
		if i%2 == 0 {
			seg(pos, math.Min(pos+d, length))
		}
		pos += d
	}
}

func (c *canvas) dot(x, y, radius float64, col color.Color) {
	const n = 20
	poly := make([][2]float64, n)
	for i := range poly {
		a := 2 * math.Pi * float64(i) / n
		poly[i] = [2]float64{x + radius*math.Cos(a), y + radius*math.Sin(a)}
	}
	c.fill(poly, col)
}

func (c *canvas) text(x, y float64, s string, face font.Face, col color.Color, a align) {
	d := &font.Drawer{Dst: c.img, Src: &image.Uniform{col}, Face: face}
	w := float64(d.MeasureString(s)) / 64
	switch a {
	case alignCentre:
		x -= w / 2
	case alignRight:
		x -= w
	}
	d.Dot = fixed.P(int(math.Round(x)), int(math.Round(y)))
	d.DrawString(s)
}
