package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"strconv"
	"time"

	"hopreact/internal/alert"
	"hopreact/internal/batteryanalysis"
	"hopreact/internal/batteryview"
	"hopreact/internal/corescope"
	"hopreact/internal/poller"
	"hopreact/internal/store"
	"hopreact/internal/weather"
)

// analyseRanges are the ranges the page offers, in days. The longest is
// limited by how long CoreScope keeps readings.
var analyseRanges = []int{3, 7, 14, 30}

const defaultAnalyseDays = 7

// analysisView is everything the Analyse battery page shows.
type analysisView struct {
	WatchID int64
	Name    string
	Days    int
	Ranges  []analyseRange

	HasPosition bool
	Position    string
	// Notices explain anything missing: no position, weather unavailable,
	// CoreScope not answering.
	Notices []string

	Verdict  batteryanalysis.Verdict
	Figures  []figure
	Chart    template.HTML
	ChartSm  template.HTML
	HoverRaw string // JSON for the hover readout
	HasTemps bool

	Ledger      []ledgerRow
	Insights    []batteryanalysis.Insight
	Forecast    template.HTML
	ForecastTxt string
	Weather     bool // weather lines drawn
	Mains       bool
}

type analyseRange struct {
	Days int
	On   bool
}

type figure struct {
	Value, Label string
}

type ledgerRow struct {
	Day, Rise, Set, Daylight, Cloud, Sun string
	SunBar                               int // px
	Gain, Overnight, Net                 string
	GainClass, OvernightClass, NetClass  string
	Suspect                              bool
}

// handleAnalyse shows one watch's battery against daylight and weather.
func (s *Server) handleAnalyse(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r.Context())
	if !ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	view, err := s.Store.WatchViewByID(r.Context(), u.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.Log.Error("loading watch", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !s.Cfg.Battery.Enabled || view.Watch.TargetKind != string(corescope.KindNode) {
		http.NotFound(w, r)
		return
	}
	days := defaultAnalyseDays
	if d, err := strconv.Atoi(r.URL.Query().Get("days")); err == nil {
		for _, ok := range analyseRanges {
			if d == ok {
				days = d
			}
		}
	}
	av, err := s.analysis(r.Context(), view, days)
	if err != nil {
		s.Log.Error("analysing battery", "watch", id, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	d := s.page(r, "Battery analysis")
	d.Watch = &view
	d.Analysis = av
	s.render(w, "analyse.html", d)
}

func (s *Server) analysis(ctx context.Context, view store.WatchView, days int) (*analysisView, error) {
	key := view.Watch.TargetKey
	now := s.Store.Now().UTC()
	loc := s.Cfg.Battery.Location()
	from := now.Add(-time.Duration(days) * 24 * time.Hour)

	av := &analysisView{WatchID: view.Watch.ID, Days: days, Name: "This repeater"}
	if view.Target != nil && view.Target.Name != "" {
		av.Name = view.Target.Name
	}
	for _, d := range analyseRanges {
		av.Ranges = append(av.Ranges, analyseRange{Days: d, On: d == days})
	}

	readings, err := s.Store.BatteryReadingsFor(ctx, []string{key})
	if err != nil {
		return nil, err
	}
	rule, _, err := s.Store.BatteryRuleFor(ctx, view.Watch.ID)
	if err != nil {
		return nil, err
	}
	levels := poller.BatteryLevelsFor(rule, s.Cfg.Battery, readings[key])

	samples := readings[key].Samples
	var temps []corescope.TempSample
	if s.Scope != nil {
		cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		rep, err := s.Scope.Battery(cctx, key, days)
		cancel()
		switch {
		case err == nil:
			samples, temps = rep.Samples, rep.Temps
		case errors.Is(err, corescope.ErrNoBattery):
			samples = nil
		default:
			s.Log.Warn("battery analysis: CoreScope failed, using stored readings", "key", key, "err", err)
			av.Notices = append(av.Notices, "CoreScope didn't answer, so this shows the readings HopReact last stored, which cover at most 7 days.")
		}
	}
	av.HasTemps = len(temps) > 0

	var lat, lon float64
	if t := view.Target; t != nil && t.Lat != nil && t.Lon != nil && !(*t.Lat == 0 && *t.Lon == 0) &&
		math.Abs(*t.Lat) <= 90 && math.Abs(*t.Lon) <= 180 {
		av.HasPosition, lat, lon = true, *t.Lat, *t.Lon
		av.Position = fmt.Sprintf("%.3f, %.3f", lat, lon)
	} else {
		av.Notices = append(av.Notices, "This repeater doesn't share its position, so there's no daylight or weather to show. "+
			"Set one on the repeater with set lat and set lon.")
	}

	var hours []weather.Hour
	if av.HasPosition && s.Cfg.Battery.Weather && s.Weather != nil {
		res, err := s.Weather.Hours(ctx, lat, lon, from.Add(-36*time.Hour), now.Add(10*24*time.Hour))
		switch {
		case errors.Is(err, weather.ErrUnavailable):
			av.Notices = append(av.Notices, "Weather is unavailable right now. Sunrise, sunset and twilight still show. Try again later.")
		case err != nil:
			return nil, err
		default:
			hours = res.Hours
			if res.Stale {
				av.Notices = append(av.Notices, "The weather service didn't answer, so the weather shown may be out of date.")
			}
		}
	}
	av.Weather = len(hours) > 0

	in := batteryanalysis.Input{Samples: samples, From: from, Now: now, HasPosition: av.HasPosition,
		Lat: lat, Lon: lon, Weather: hours, Levels: levels, Loc: loc}
	res := batteryanalysis.Analyse(in)
	av.Verdict = res.Verdict(loc)
	av.Mains = res.Mains
	av.Figures = figures(res, now, loc, lat, lon, av.HasPosition)
	if av.HasPosition {
		av.Insights = res.Insights(now, loc, lat, lon)
	}
	av.Ledger = ledgerRows(res.Days, now, loc)

	ch := chartInput{Samples: within(samples, from, now), Temps: temps, Hours: hours, Spans: res.Spans,
		From: from, To: now, Levels: levels, Loc: loc}
	av.Chart = analysisChart(ch, false)
	av.ChartSm = analysisChart(ch, true)
	av.HoverRaw = hoverJSON(ch)
	if len(res.Forecast) > 0 {
		av.Forecast = forecastChart(res.Forecast, loc)
		av.ForecastTxt = res.ForecastText(loc)
	}
	return av, nil
}

func within(s []alert.Sample, from, to time.Time) []alert.Sample {
	var out []alert.Sample
	for _, x := range s {
		if !x.At.Before(from) && !x.At.After(to) {
			out = append(out, x)
		}
	}
	return out
}

func figures(r batteryanalysis.Result, now time.Time, loc *time.Location, lat, lon float64, hasPos bool) []figure {
	var out []figure
	if r.LatestMV > 0 {
		out = append(out, figure{batteryview.Volts(r.LatestMV), "Now"})
	}
	if r.HasNet {
		out = append(out, figure{fmt.Sprintf("%+d mV", r.NetMV), "Over " + spanDays(r.NetSpan)})
	}
	if r.Fit != nil {
		v := fmt.Sprintf("%.2f kWh/m²", r.Fit.BreakEvenKWh)
		if r.Fit.BreakEvenKWh <= 0 {
			v = "Any day"
		}
		out = append(out, figure{v, "Sun to break even"})
	}
	if hasPos {
		if s, ok := batteryanalysis.Season(now, loc, lat, lon); ok {
			out = append(out, figure{hoursMinutes(s.Today), "Daylight today"})
			m := int(math.Round(math.Abs(s.Change.Minutes())))
			switch {
			case s.Change < 0:
				out = append(out, figure{fmt.Sprintf("%d min", m), "Shorter each day"})
			case s.Change > 0:
				out = append(out, figure{fmt.Sprintf("%d min", m), "Longer each day"})
			}
		}
	}
	return out
}

func spanDays(d time.Duration) string {
	if d < 36*time.Hour {
		return fmt.Sprintf("%d hours", int(math.Round(d.Hours())))
	}
	return fmt.Sprintf("%d days", int(math.Round(d.Hours()/24)))
}

func hoursMinutes(d time.Duration) string {
	m := int(math.Round(d.Minutes()))
	return fmt.Sprintf("%d h %02d m", m/60, m%60)
}

func ledgerRows(days []batteryanalysis.Day, now time.Time, loc *time.Location) []ledgerRow {
	maxSun := 0.0
	for _, d := range days {
		maxSun = math.Max(maxSun, d.SunKWh)
	}
	mv := func(has bool, v int) (string, string) {
		if !has {
			return "–", ""
		}
		if v >= 0 {
			return fmt.Sprintf("+%d mV", v), "pos"
		}
		return fmt.Sprintf("−%d mV", -v), "neg"
	}
	var out []ledgerRow
	for i := len(days) - 1; i >= 0; i-- { // newest first
		d := days[i]
		row := ledgerRow{
			Day:      d.Rise.In(loc).Format("Mon 2 Jan"),
			Rise:     d.Rise.In(loc).Format("15:04"),
			Set:      d.Set.In(loc).Format("15:04"),
			Daylight: hoursMinutes(d.Daylight()),
			Cloud:    "–", Sun: "–",
			Suspect: d.PanelSuspect,
		}
		if d.HasWeather {
			row.Cloud = fmt.Sprintf("%d%%", int(math.Round(d.CloudPct)))
			row.Sun = fmt.Sprintf("%.2f", d.SunKWh)
			if maxSun > 0 {
				row.SunBar = int(math.Round(d.SunKWh / maxSun * 48))
			}
		}
		row.Gain, row.GainClass = mv(d.HasGain, d.GainMV)
		row.Overnight, row.OvernightClass = mv(d.HasOvernight, d.OvernightMV)
		row.Net, row.NetClass = mv(d.HasNet, d.NetMV)
		if !d.HasNet && d.NextRise.Add(3*time.Hour).After(now) {
			row.Net = "tomorrow"
			if !d.HasOvernight {
				row.Overnight = "tonight"
			}
			if !d.HasGain && d.Set.Add(2*time.Hour).After(now) {
				row.Gain = "today"
			}
		}
		out = append(out, row)
	}
	return out
}

// hoverJSON is the data the page script uses for its readout, kept small:
// times in unix seconds, voltages in mV.
func hoverJSON(c chartInput) string {
	type span struct {
		S, E  int64
		Phase string
	}
	h := struct {
		T0, T1    int64
		L, R, W   float64
		Readings  [][2]int64
		Hours     [][4]float64
		Temps     [][2]float64
		Spans     []span
		TZOffsets [][2]int64
	}{T0: c.From.Unix(), T1: c.To.Unix(), L: chartLeft, R: chartRight, W: chartWide}
	for _, x := range c.Samples {
		h.Readings = append(h.Readings, [2]int64{x.At.Unix(), int64(x.MV)})
	}
	for _, x := range c.Hours {
		if !x.At.Before(c.From) && !x.At.After(c.To.Add(time.Hour)) {
			h.Hours = append(h.Hours, [4]float64{float64(x.At.Unix()), math.Round(x.SunWm2), math.Round(x.CloudPct), math.Round(x.TempC*10) / 10})
		}
	}
	for _, x := range c.Temps {
		if !x.At.Before(c.From) && !x.At.After(c.To) {
			h.Temps = append(h.Temps, [2]float64{float64(x.At.Unix()), x.C})
		}
	}
	for _, x := range c.Spans {
		h.Spans = append(h.Spans, span{x.Start.Unix(), x.End.Unix(), x.Phase.Name()})
	}
	// The browser's own timezone may differ from the site's, so the page is
	// told the site's offset at each change instead.
	_, off := c.From.In(c.Loc).Zone()
	h.TZOffsets = append(h.TZOffsets, [2]int64{c.From.Unix(), int64(off)})
	for t := c.From; t.Before(c.To); t = t.Add(time.Hour) {
		if _, o := t.In(c.Loc).Zone(); o != off {
			off = o
			h.TZOffsets = append(h.TZOffsets, [2]int64{t.Unix(), int64(o)})
		}
	}
	b, _ := json.Marshal(h)
	return string(b)
}
