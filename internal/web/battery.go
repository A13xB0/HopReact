package web

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"hopreact/internal/alert"
	"hopreact/internal/batteryview"
	"hopreact/internal/config"
	"hopreact/internal/corescope"
	"hopreact/internal/poller"
	"hopreact/internal/store"
)

// batteryCard is everything the pages show about one watch's battery.
type batteryCard struct {
	WatchID int64
	HasRule bool
	Rule    store.BatteryRule
	// HasReading is set when CoreScope has a real battery reading for it.
	HasReading bool
	NoBattery  bool // reports 0 mV: no battery fitted
	// Silent means CoreScope knows the repeater but has had no reading from
	// it in the last week, which is what a flat battery looks like.
	Silent bool
	// Outdated is when the stored reading was last refreshed, set when that
	// was too long ago to show it.
	Outdated time.Time
	Stale    bool
	MV       int
	ReadAt   time.Time
	Reporter string
	Levels   alert.BatteryLevels
	// SiteLowMV, SiteCriticalMV and SiteOverMV are what blank fields mean.
	SiteLowMV      int
	SiteCriticalMV int
	SiteOverMV     int
	Trend          alert.Trend
	Headline       string
	Detail         string
	Ahead          []batteryview.Ahead
	Spark          template.HTML
	// ScopeURL links to the battery chart on CoreScope.
	ScopeURL string
	// Loc is the battery timezone, for the times shown.
	Loc *time.Location
}

// Class is the pill colour for the current level: the rule's judged level
// when it has one, otherwise the reading against the levels.
func (b *batteryCard) Class() string {
	if !b.HasReading {
		return "muted"
	}
	if b.HasRule {
		switch b.Rule.Level {
		case "critical", "overcharged":
			return "bad"
		case "low":
			return "warn"
		case "ok":
			return "good"
		}
	}
	switch {
	case b.MV < b.Levels.CriticalMV:
		return "bad"
	case b.MV < b.Levels.LowMV:
		return "warn"
	case b.Levels.OverchargeMV > 0 && b.MV >= b.Levels.OverchargeMV:
		return "bad"
	}
	return "good"
}

// Chip is the short label for the watches list.
func (b *batteryCard) Chip() string {
	switch {
	case b.NoBattery:
		return "no battery"
	case !b.HasReading:
		return "—"
	}
	v := batteryview.Volts(b.MV)
	switch b.Class() {
	case "warn":
		return v + " low"
	case "bad":
		judged := b.HasRule && b.Rule.Level != "unknown" && b.Rule.Level != ""
		if judged && b.Rule.Level == "overcharged" ||
			!judged && b.Levels.OverchargeMV > 0 && b.MV >= b.Levels.OverchargeMV {
			return v + " overcharged"
		}
		return v + " critical"
	}
	return v
}

// Short is the one-line trend for the watches list.
func (b *batteryCard) Short() string {
	if !b.HasReading {
		return ""
	}
	if len(b.Ahead) > 0 && b.Trend.Kind == alert.TrendDraining {
		a := b.Ahead[0]
		return "↓ " + a.Name + " " + a.Value
	}
	return b.Headline
}

// StateLabel is the pill text on the watch page.
func (b *batteryCard) StateLabel() string {
	if !b.HasRule {
		return "Off"
	}
	since := b.Rule.Since.In(b.loc()).Format("Mon 15:04")
	switch b.Rule.Level {
	case "ok":
		return "OK"
	case "low":
		return "Low · since " + since
	case "critical":
		return "Critical · since " + since
	case "overcharged":
		return "Overcharged · since " + since
	}
	switch {
	case b.NoBattery:
		return "No battery"
	case b.Silent, !b.Outdated.IsZero():
		return "No recent reading"
	case b.HasReading:
		return "Judged at the next check"
	}
	return "Waiting for a reading"
}

func (b *batteryCard) loc() *time.Location {
	if b.Loc == nil {
		return time.UTC
	}
	return b.Loc
}

// Volts is a template helper for showing a level.
func volts(mv int) string { return batteryview.Volts(mv) }

// voltsField fills a level box: blank when the watch follows the site.
func voltsField(mv int) string {
	if mv == 0 {
		return ""
	}
	return fmt.Sprintf("%.2f", float64(mv)/1000)
}

// batteryCards builds the battery view for each of one person's watches.
func (s *Server) batteryCards(ctx context.Context, userID int64, watches []store.Watch) (map[int64]*batteryCard, error) {
	out := map[int64]*batteryCard{}
	if !s.Cfg.Battery.Enabled {
		return out, nil
	}
	rules, err := s.Store.BatteryRulesForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(watches))
	for _, w := range watches {
		keys = append(keys, w.TargetKey)
	}
	readings, err := s.Store.BatteryReadingsFor(ctx, keys)
	if err != nil {
		return nil, err
	}
	now := s.Store.Now().UTC()
	loc := s.Cfg.Battery.Location()
	for _, w := range watches {
		if w.TargetKind != string(corescope.KindNode) {
			continue
		}
		rule, has := rules[w.ID]
		rd := readings[w.TargetKey]
		// A stored reading stops updating when its repeater no longer gets
		// fetched (no rule and none due) or CoreScope keeps failing for it.
		// Past a few checks it is too old to show as current.
		var outdated time.Time
		if !rd.FetchedAt.IsZero() && now.Sub(rd.FetchedAt) > 3*s.Cfg.Battery.CheckEvery {
			outdated = rd.FetchedAt
			rd = store.BatteryReadings{}
		}
		c := &batteryCard{
			WatchID: w.ID, HasRule: has, Rule: rule,
			HasReading: rd.Status.HasBattery(), NoBattery: rd.Status == corescope.BatteryNone,
			Silent:   rd.Status == corescope.BatteryUnknown,
			Outdated: outdated,
			Stale:    rd.Stale, MV: rd.LatestMV, ReadAt: rd.LatestAt, Reporter: rd.Reporter,
			Levels: poller.BatteryLevelsFor(rule, s.Cfg.Battery, rd),
			Loc:    loc,
		}
		site := poller.BatteryLevelsFor(store.BatteryRule{OverchargeOn: true}, s.Cfg.Battery, rd)
		c.SiteLowMV, c.SiteCriticalMV, c.SiteOverMV = site.LowMV, site.CriticalMV, site.OverchargeMV
		if s.Cfg.CoreScope.APIURL != "" {
			c.ScopeURL = strings.TrimRight(s.Cfg.CoreScope.APIURL, "/") + "/#/nodes/" + w.TargetKey + "/analytics"
		}
		if c.HasReading {
			samples := rd.Samples
			c.Trend = alert.AnalyseTrend(samples, now, loc)
			c.Headline = batteryview.Headline(c.Trend)
			c.Detail = batteryview.Detail(c.Trend)
			c.Ahead = batteryview.AheadOf(c.Trend, c.Levels, now, loc)
			c.Spark = sparkline(samples, now, c.Levels)
		}
		out[w.ID] = c
	}
	return out, nil
}

// sparkline draws a week of readings as a small inline SVG with the low and
// critical lines. Built from numbers only, so it is safe to mark as HTML.
func sparkline(samples []alert.Sample, now time.Time, lv alert.BatteryLevels) template.HTML {
	const w, h = 300.0, 80.0
	t0 := now.Add(-7 * 24 * time.Hour)
	lo, hi := float64(lv.CriticalMV)-100, float64(lv.LowMV)+100
	var pts []alert.Sample
	for _, x := range samples {
		if x.At.Before(t0) || x.At.After(now) {
			continue
		}
		pts = append(pts, x)
		lo, hi = math.Min(lo, float64(x.MV)-30), math.Max(hi, float64(x.MV)+30)
	}
	if len(pts) < 2 {
		return ""
	}
	x := func(t time.Time) float64 { return 4 + float64(t.Sub(t0))/float64(now.Sub(t0))*(w-8) }
	y := func(mv float64) float64 { return h - 4 - (mv-lo)/(hi-lo)*(h-8) }
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="spark" viewBox="0 0 %.0f %.0f" role="img" aria-label="Battery over the last 7 days, now %s">`,
		w, h, batteryview.Volts(pts[len(pts)-1].MV))
	fmt.Fprintf(&b, `<line x1="0" y1="%.1f" x2="%.0f" y2="%.1f" stroke="#facc15" stroke-width="1.2" stroke-dasharray="5 4"/>`,
		y(float64(lv.LowMV)), w, y(float64(lv.LowMV)))
	fmt.Fprintf(&b, `<line x1="0" y1="%.1f" x2="%.0f" y2="%.1f" stroke="#f87171" stroke-width="1.2" stroke-dasharray="5 4"/>`,
		y(float64(lv.CriticalMV)), w, y(float64(lv.CriticalMV)))
	b.WriteString(`<polyline fill="none" stroke="#5aa9ff" stroke-width="2" stroke-linejoin="round" points="`)
	for i, p := range pts {
		if i > 0 {
			b.WriteString(" ")
		}
		fmt.Fprintf(&b, "%.1f,%.1f", x(p.At), y(float64(p.MV)))
	}
	b.WriteString(`"/>`)
	last := pts[len(pts)-1]
	fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="3.5" fill="#ffffff"/></svg>`, x(last.At), y(float64(last.MV)))
	return template.HTML(b.String())
}

// parseVolts reads a level box: blank is 0 ("follow the site"), anything
// else must be a voltage in range.
func parseVolts(v string) (int, error) {
	v = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(v), "V"))
	if v == "" {
		return 0, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(f) {
		return 0, fmt.Errorf("%q isn't a voltage. Enter it like 3.65", v)
	}
	mv := int(math.Round(f * 1000))
	if mv < config.MinBatteryMV || mv > config.MaxBatteryMV {
		return 0, fmt.Errorf("%.2f V is outside %.2f–%.2f V", f, float64(config.MinBatteryMV)/1000, float64(config.MaxBatteryMV)/1000)
	}
	return mv, nil
}

// handleBattery changes one watch's battery alerts: on, off, its levels, or
// the overcharge alert.
func (s *Server) handleBattery(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r.Context())
	if !ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	dest := fmt.Sprintf("/watches/%d", id)
	watch, err := s.Store.WatchOwnedBy(r.Context(), u.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.Log.Error("checking watch ownership", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !s.Cfg.Battery.Enabled || watch.TargetKind != string(corescope.KindNode) {
		redirectFlash(w, r, dest, "Battery alerts aren't available for this one.", "error")
		return
	}

	var msg string
	switch r.PostFormValue("action") {
	case "on":
		err, msg = s.Store.AddBatteryRule(r.Context(), u.ID, id), "Battery alerts are on for this repeater."
	case "off":
		err, msg = s.Store.DeleteBatteryRule(r.Context(), u.ID, id), "Battery alerts are off for this repeater."
	case "overcharge_on", "overcharge_off":
		var rule store.BatteryRule
		var has bool
		if rule, has, err = s.Store.BatteryRuleFor(r.Context(), id); err != nil {
			break
		}
		if !has {
			err = store.ErrNotFound
			break
		}
		on := r.PostFormValue("action") == "overcharge_on"
		if on {
			readings, rerr := s.Store.BatteryReadingsFor(r.Context(), []string{watch.TargetKey})
			if rerr != nil {
				err = rerr
				break
			}
			rule.OverchargeOn = true
			lv := poller.BatteryLevelsFor(rule, s.Cfg.Battery, readings[watch.TargetKey])
			if lv.OverchargeMV <= lv.LowMV {
				redirectFlash(w, r, dest, fmt.Sprintf("The overcharge level (%s) must be above low (%s). Change it under Edit levels.",
					volts(lv.OverchargeMV), volts(lv.LowMV)), "error")
				return
			}
		}
		err = s.Store.UpdateBatteryLevels(r.Context(), u.ID, id, rule.LowMV, rule.CriticalMV, on, rule.OverchargeMV)
		msg = "Overcharge alert turned off."
		if on {
			msg = "Overcharge alert turned on."
		}
	case "levels":
		msg, err = s.saveBatteryLevels(r, u.ID, watch)
		if err == nil && msg != "" && !strings.HasPrefix(msg, "Saved") {
			redirectFlash(w, r, dest, msg, "error")
			return
		}
	default:
		redirectFlash(w, r, dest, "That battery change isn't valid.", "error")
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		redirectFlash(w, r, dest, "Turn battery alerts on first.", "error")
		return
	}
	if err != nil {
		s.Log.Error("updating battery alerts", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	redirectFlash(w, r, dest, msg, "ok")
}

// saveBatteryLevels validates and stores the level boxes. A message not
// starting "Saved" is a validation problem to show the person.
func (s *Server) saveBatteryLevels(r *http.Request, userID int64, watch store.Watch) (string, error) {
	low, err := parseVolts(r.PostFormValue("low"))
	if err != nil {
		return "Low: " + err.Error() + ".", nil
	}
	crit, err := parseVolts(r.PostFormValue("critical"))
	if err != nil {
		return "Critical: " + err.Error() + ".", nil
	}
	over, err := parseVolts(r.PostFormValue("overcharge_level"))
	if err != nil {
		return "Overcharge: " + err.Error() + ".", nil
	}
	overOn := r.PostFormValue("overcharge") == "on"

	// Judge the levels that will actually apply, blanks filled from the site.
	readings, err := s.Store.BatteryReadingsFor(r.Context(), []string{watch.TargetKey})
	if err != nil {
		return "", err
	}
	lv := poller.BatteryLevelsFor(store.BatteryRule{LowMV: low, CriticalMV: crit, OverchargeOn: overOn, OverchargeMV: over},
		s.Cfg.Battery, readings[watch.TargetKey])
	if lv.CriticalMV >= lv.LowMV {
		return fmt.Sprintf("Critical (%s) must be lower than low (%s).", volts(lv.CriticalMV), volts(lv.LowMV)), nil
	}
	if overOn && lv.OverchargeMV <= lv.LowMV {
		return fmt.Sprintf("The overcharge level (%s) must be above low (%s).", volts(lv.OverchargeMV), volts(lv.LowMV)), nil
	}
	if err := s.Store.UpdateBatteryLevels(r.Context(), userID, watch.ID, low, crit, overOn, over); err != nil {
		return "", err
	}
	return "Saved. New levels apply from the next reading.", nil
}

// handleAutoBattery chooses whether battery rules are added automatically.
func (s *Server) handleAutoBattery(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r.Context())
	if !ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	on := r.PostFormValue("auto_battery") == "on"
	if err := s.Store.SetAutoBattery(r.Context(), u.ID, on); err != nil {
		s.Log.Error("updating battery preference", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	msg := "Battery alerts will be added to your repeaters when they report a battery."
	if !on {
		msg = "Battery alerts won't be added automatically. The ones you have stay."
	}
	redirectFlash(w, r, "/watches", msg, "ok")
}

// handleBatteryBanner hides the one-off "battery alerts are on" banner.
func (s *Server) handleBatteryBanner(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r.Context())
	if !ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if err := s.Store.DismissBatteryBanner(r.Context(), u.ID); err != nil {
		s.Log.Error("dismissing battery banner", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/watches", http.StatusSeeOther)
}
