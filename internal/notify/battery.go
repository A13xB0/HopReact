package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strings"
	"time"

	"hopreact/internal/alert"
	"hopreact/internal/batteryanalysis"
	"hopreact/internal/batteryview"
	"hopreact/internal/corescope"
	"hopreact/internal/discord"
	"hopreact/internal/poller"
	"hopreact/internal/sun"
)

// maxBatteryCharts is how many battery cards with a chart one message
// carries. Discord allows ten; past four, a bad night would be a wall of
// charts, so the rest are listed as text.
const maxBatteryCharts = 4

// Embed edge colours.
const (
	colourLow      = 0xfacc15
	colourCritical = 0xf87171
	colourOK       = 0x4ade80
)

var batteryOrder = map[string]int{"critical": 0, "overcharged": 1, "low": 2, "recovered": 3}

var batteryHeadings = map[string][2]string{
	// one, several
	"low":         {"🪫 **A repeater's battery is getting low**", "🪫 **%d repeaters' batteries are getting low**"},
	"critical":    {"🔴 **A repeater's battery is critical**", "🔴 **%d repeaters' batteries are critical**"},
	"overcharged": {"⚠️ **A repeater's battery is overcharged**", "⚠️ **%d repeaters' batteries are overcharged**"},
	"recovered":   {"🟢 **Battery OK again**", "🟢 **%d batteries are OK again**"},
}

// batterySection renders battery alerts: a heading per kind of change, a
// card with a chart for each of the first few alerts, and text lines for the
// rest and for anything whose chart could not be drawn.
func (n *Notifier) batterySection(ctx context.Context, userID int64, items []poller.BatteryPayload, now time.Time) (string, []discord.Embed, []discord.File) {
	sort.SliceStable(items, func(i, j int) bool {
		if batteryOrder[items[i].Level] != batteryOrder[items[j].Level] {
			return batteryOrder[items[i].Level] < batteryOrder[items[j].Level]
		}
		return items[i].TargetName < items[j].TargetName
	})
	loc := n.Loc
	if loc == nil {
		loc = time.UTC
	}

	var b strings.Builder
	var embeds []discord.Embed
	var files []discord.File
	byLevel := map[string][]poller.BatteryPayload{}
	var levels []string
	for _, it := range items {
		if _, seen := byLevel[it.Level]; !seen {
			levels = append(levels, it.Level)
		}
		byLevel[it.Level] = append(byLevel[it.Level], it)
	}
	for _, lvl := range levels {
		group := byLevel[lvl]
		h := batteryHeadings[lvl]
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		if len(group) == 1 {
			b.WriteString(h[0] + "\n")
		} else {
			fmt.Fprintf(&b, h[1]+"\n", len(group))
		}
		for _, it := range group {
			samples := corescope.UnpackSamples(it.Samples)
			// As of when the alert fired, so a retried message tells the same
			// story as it would have on time.
			at := now
			if it.FiredUnix > 0 {
				at = time.Unix(it.FiredUnix, 0).UTC()
			}
			tr := alert.AnalyseTrend(samples, at, loc)
			lv := alert.BatteryLevels{LowMV: it.LowMV, CriticalMV: it.CriticalMV, OverchargeMV: it.OverchargeMV}
			if lvl != "recovered" && len(embeds) < maxBatteryCharts {
				lat, lon, hasPos := n.position(ctx, it.TargetKey)
				var spans []sun.Span
				if hasPos {
					// The chart runs from a week back to at most 8 days ahead.
					spans = sun.Spans(at.Add(-7*24*time.Hour), at.Add(8*24*time.Hour), lat, lon)
				}
				png, err := BatteryChart(ChartInput{Name: it.TargetName, Samples: samples, Now: at, Trend: tr, Levels: lv, Loc: loc, Spans: spans})
				if err == nil {
					name := fmt.Sprintf("battery-%d.png", len(files)+1)
					files = append(files, discord.File{Name: name, ContentType: "image/png", Data: png})
					e := n.batteryEmbed(it, tr, lv, at, loc, name)
					if hasPos && (lvl == "low" || lvl == "critical") {
						if txt := n.weekAhead(ctx, it.TargetKey, samples, lv, at, loc, lat, lon); txt != "" {
							e.Fields = append(e.Fields, discord.EmbedField{Name: "Next 7 days", Value: txt})
						}
					}
					if link := n.analyseLink(ctx, userID, it.TargetKey); link != "" {
						e.Fields = append(e.Fields, discord.EmbedField{Name: "More", Value: "[Analyse battery](" + link + ")"})
					}
					embeds = append(embeds, e)
					continue
				}
				n.Log.Warn("battery chart not drawn, sending text", "target", it.TargetKey, "err", err)
			}
			b.WriteString(batteryLine(it, tr, lv, at, loc))
		}
	}
	return b.String(), embeds, files
}

// forecastHistoryDays is how far back an alert's forecast learns from.
const forecastHistoryDays = 30

// position is where a repeater says it is, if it says.
func (n *Notifier) position(ctx context.Context, key string) (float64, float64, bool) {
	t, err := n.Store.Target(ctx, string(corescope.KindNode), key)
	if err != nil || t.Lat == nil || t.Lon == nil || (*t.Lat == 0 && *t.Lon == 0) ||
		math.Abs(*t.Lat) > 90 || math.Abs(*t.Lon) > 180 {
		return 0, 0, false
	}
	return *t.Lat, *t.Lon, true
}

// weekAhead is the forecast sentence for an alert, or "" when there's no
// weather or not enough history for a break-even yet.
func (n *Notifier) weekAhead(ctx context.Context, key string, samples []alert.Sample, lv alert.BatteryLevels,
	at time.Time, loc *time.Location, lat, lon float64) string {
	if n.Weather == nil {
		return ""
	}
	wctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	res, err := n.Weather.Hours(wctx, lat, lon, at.Add(-9*24*time.Hour), at.Add(10*24*time.Hour))
	if err != nil {
		n.Log.Warn("no weather for battery alert", "err", err)
		return ""
	}
	learnFrom := at.Add(-7 * 24 * time.Hour)
	if n.Scope != nil {
		// As much history as CoreScope has, so the break-even is as good as
		// it can be. The week in the alert is the fallback.
		if rep, err := n.Scope.Battery(wctx, key, forecastHistoryDays); err == nil && len(rep.Samples) > len(samples) {
			samples = rep.Samples
			learnFrom = at.Add(-forecastHistoryDays * 24 * time.Hour)
			if wx, err := n.Weather.Hours(wctx, lat, lon, learnFrom.Add(-36*time.Hour), at.Add(10*24*time.Hour)); err == nil {
				res = wx
			}
		}
	}
	r := batteryanalysis.Analyse(batteryanalysis.Input{Samples: samples, From: at.Add(-7 * 24 * time.Hour), Now: at,
		HasPosition: true, Lat: lat, Lon: lon, Weather: res.Hours, Levels: lv, Loc: loc, FitFrom: learnFrom})
	return r.ForecastText(loc)
}

// analyseLink is the person's Analyse battery page for a repeater, if they
// still watch it.
func (n *Notifier) analyseLink(ctx context.Context, userID int64, key string) string {
	if n.BaseURL == "" {
		return ""
	}
	id, ok, err := n.Store.WatchIDFor(ctx, userID, string(corescope.KindNode), key)
	if err != nil || !ok {
		return ""
	}
	return fmt.Sprintf("%s/watches/%d/battery/analyse", strings.TrimRight(n.BaseURL, "/"), id)
}

// batteryLine is one alert as text, for alerts without a card.
func batteryLine(it poller.BatteryPayload, tr alert.Trend, lv alert.BatteryLevels, now time.Time, loc *time.Location) string {
	var b strings.Builder
	fmt.Fprintf(&b, "• **%s** (repeater): %s", it.TargetName, batteryview.Volts(it.MV))
	switch it.Level {
	case "low":
		fmt.Fprintf(&b, ", low is below %s.", batteryview.Volts(it.LowMV))
	case "critical":
		fmt.Fprintf(&b, ", critical is below %s.", batteryview.Volts(it.CriticalMV))
	case "overcharged":
		fmt.Fprintf(&b, ", your limit is %s. A full cell is 4.20 V, so the charger or solar controller may be faulty.", batteryview.Volts(it.OverchargeMV))
	default:
		b.WriteString(".")
	}
	if it.Level == "low" || it.Level == "critical" {
		if ahead := batteryview.AheadOf(tr, lv, now, loc); len(ahead) > 0 {
			fmt.Fprintf(&b, " %s %s.", ahead[0].Label, ahead[0].Value)
		}
	}
	b.WriteString("\n")
	return b.String()
}

func (n *Notifier) batteryEmbed(it poller.BatteryPayload, tr alert.Trend, lv alert.BatteryLevels, now time.Time, loc *time.Location, file string) discord.Embed {
	e := discord.Embed{
		Title: it.TargetName + " (repeater)",
		Image: &discord.EmbedImage{URL: "attachment://" + file},
		Color: colourLow,
	}
	if n.ScopeURL != "" {
		e.URL = strings.TrimRight(n.ScopeURL, "/") + "/#/nodes/" + url.PathEscape(it.TargetKey) + "/analytics"
	}
	reading := batteryview.Volts(it.MV)
	switch it.Level {
	case "low":
		reading += " · low is below " + batteryview.Volts(it.LowMV)
	case "critical":
		e.Color = colourCritical
		reading += " · critical is below " + batteryview.Volts(it.CriticalMV)
	case "overcharged":
		e.Color = colourCritical
		reading += " · your limit is " + batteryview.Volts(it.OverchargeMV)
		e.Description = "A full cell is 4.20 V, so the charger or solar controller may be faulty. Check it or disconnect the battery."
	}
	e.Fields = append(e.Fields,
		discord.EmbedField{Name: "Battery", Value: reading, Inline: true},
		discord.EmbedField{Name: "Trend", Value: batteryview.Headline(tr), Inline: true})
	for _, a := range batteryview.AheadOf(tr, lv, now, loc) {
		// While low, the low level is behind it, and AheadOf leaves it out.
		e.Fields = append(e.Fields, discord.EmbedField{Name: a.Label, Value: a.Value, Inline: true})
	}
	footer := ""
	if it.ReadUnix > 0 {
		footer = "Read " + time.Unix(it.ReadUnix, 0).In(loc).Format("15:04 Mon")
		if it.Reporter != "" {
			footer += " by " + it.Reporter
		}
	}
	if tr.Kind == alert.TrendDraining || tr.Kind == alert.TrendCharging {
		footer += " · shaded area is the likely range"
	}
	if it.Level == "critical" {
		footer += " · act today: change the battery or check the panel"
	}
	if footer = strings.TrimPrefix(footer, " · "); footer != "" {
		e.Footer = &discord.EmbedFooter{Text: footer}
	}
	return e
}

// batteryEnabledSection tells someone which of their repeaters were given a
// battery rule.
func batteryEnabledSection(msgs []poller.BatteryEnabledPayload) string {
	var items []poller.BatteryEnabledItem
	low, crit := 0, 0
	for _, m := range msgs {
		items = append(items, m.Repeaters...)
		low, crit = m.LowMV, m.CriticalMV
	}
	if len(items) == 0 {
		return ""
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	var b strings.Builder
	if len(items) == 1 {
		b.WriteString("🔋 **Battery alerts are now on for 1 of your repeaters**\n")
	} else {
		fmt.Fprintf(&b, "🔋 **Battery alerts are now on for %d of your repeaters**\n", len(items))
	}
	for _, it := range items {
		fmt.Fprintf(&b, "• **%s**: %s", it.Name, batteryview.Volts(it.MV))
		switch it.Level {
		case "low":
			b.WriteString(", already low")
		case "critical":
			b.WriteString(", already critical")
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "You'll get a message if one drops below %s, and again below %s. "+
		"Turn this off for any repeater on its page.\n", batteryview.Volts(low), batteryview.Volts(crit))
	return b.String()
}

func decodeBattery(raw string) (poller.BatteryPayload, bool) {
	var p poller.BatteryPayload
	return p, json.Unmarshal([]byte(raw), &p) == nil
}

func decodeBatteryEnabled(raw string) (poller.BatteryEnabledPayload, bool) {
	var p poller.BatteryEnabledPayload
	return p, json.Unmarshal([]byte(raw), &p) == nil
}
