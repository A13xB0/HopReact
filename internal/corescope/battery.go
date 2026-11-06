package corescope

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"time"
)

// ErrNoBattery is returned when CoreScope has no battery readings for a node
// at all. It answers 404 for a node nobody has reported, which is the normal
// case for most repeaters, not a failure.
var ErrNoBattery = errors.New("corescope: no battery readings for this node")

// BatteryStatus is CoreScope's own grading of the latest reading.
type BatteryStatus string

const (
	BatteryOK       BatteryStatus = "ok"
	BatteryLow      BatteryStatus = "low"
	BatteryCritical BatteryStatus = "critical"
	BatteryNone     BatteryStatus = "no_battery"
	BatteryUnknown  BatteryStatus = "unknown"
)

// HasBattery reports whether a status means a battery is fitted and reading.
// 0 mV ("no_battery") is what a mains or solar node without a sensor sends.
func (s BatteryStatus) HasBattery() bool {
	return s == BatteryOK || s == BatteryLow || s == BatteryCritical
}

// BatterySample is one reading.
type BatterySample struct {
	At time.Time
	MV int
}

// PackSamples turns readings into the [unix seconds, mV] pairs they are
// stored and queued as. UnpackSamples is the reverse. One pair of helpers, so
// the stored readings and the queued alerts can't drift apart.
func PackSamples(s []BatterySample) [][2]int64 {
	out := make([][2]int64, 0, len(s))
	for _, x := range s {
		out = append(out, [2]int64{x.At.Unix(), int64(x.MV)})
	}
	return out
}

func UnpackSamples(p [][2]int64) []BatterySample {
	out := make([]BatterySample, 0, len(p))
	for _, x := range p {
		out = append(out, BatterySample{At: time.Unix(x[0], 0).UTC(), MV: int(x[1])})
	}
	return out
}

// TempSample is the repeater's own temperature, sent with some readings.
type TempSample struct {
	At time.Time
	C  float64
}

// BatteryReport is what CoreScope knows about one node's battery.
type BatteryReport struct {
	Status   BatteryStatus
	LatestMV int
	LatestAt time.Time
	// Reporter names the station whose reading LatestMV is.
	Reporter string
	// Stale means the latest reading is older than CoreScope's own cutoff.
	Stale bool
	// LowMV and CriticalMV are CoreScope's configured thresholds.
	LowMV      int
	CriticalMV int
	// Samples are every reading in the window from every source, oldest
	// first, one per timestamp. Different stations read the same battery, so
	// they belong on one line.
	Samples []BatterySample
	// Temps are the readings that carried a temperature, oldest first.
	Temps []TempSample
}

type batteryJSON struct {
	Status   string  `json:"status"`
	LatestMV *int    `json:"latest_mv"`
	LatestTS *string `json:"latest_ts"`
	Stale    bool    `json:"stale"`
	Selected *struct {
		ObserverName string `json:"observer_name"`
		Source       string `json:"source"`
	} `json:"selected"`
	Thresholds struct {
		LowMV      int `json:"low_mv"`
		CriticalMV int `json:"critical_mv"`
	} `json:"thresholds"`
	Samples []struct {
		Timestamp   *string  `json:"timestamp"`
		BatteryMV   int      `json:"battery_mv"`
		Temperature *float64 `json:"temperature_c"`
	} `json:"samples"`
}

// Battery fetches a node's battery readings for the last days days.
func (c *Client) Battery(ctx context.Context, key string, days int) (BatteryReport, error) {
	u := fmt.Sprintf("%s/api/nodes/%s/battery?days=%d", c.BaseURL, url.PathEscape(key), days)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return BatteryReport{}, fmt.Errorf("corescope: building request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return BatteryReport{}, fmt.Errorf("corescope: GET %s: %w", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return BatteryReport{}, ErrNoBattery
	}
	if resp.StatusCode != http.StatusOK {
		return BatteryReport{}, fmt.Errorf("corescope: GET %s: status %d", u, resp.StatusCode)
	}
	var raw batteryJSON
	// A week of hourly readings is tens of kilobytes; the cap only stops a
	// misbehaving server from filling memory.
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&raw); err != nil {
		return BatteryReport{}, fmt.Errorf("corescope: decoding %s: %w", u, err)
	}
	return raw.report(), nil
}

func (raw batteryJSON) report() BatteryReport {
	r := BatteryReport{
		Status:     BatteryStatus(raw.Status),
		LatestAt:   parseTime(raw.LatestTS),
		Stale:      raw.Stale,
		LowMV:      raw.Thresholds.LowMV,
		CriticalMV: raw.Thresholds.CriticalMV,
	}
	if raw.LatestMV != nil {
		r.LatestMV = *raw.LatestMV
	}
	if raw.Selected != nil {
		r.Reporter = raw.Selected.ObserverName
	}
	seen := map[int64]bool{}
	seenTemp := map[int64]bool{}
	for _, s := range raw.Samples {
		at := parseTime(s.Timestamp)
		// A board's sensor reads well outside this only when it is broken.
		if !at.IsZero() && s.Temperature != nil && *s.Temperature > -40 && *s.Temperature < 85 && !seenTemp[at.Unix()] {
			seenTemp[at.Unix()] = true
			r.Temps = append(r.Temps, TempSample{At: at, C: *s.Temperature})
		}
		// 0 mV means no battery fitted, and a reading without a time cannot
		// be placed on a trend.
		if at.IsZero() || s.BatteryMV <= 0 || seen[at.Unix()] {
			continue
		}
		seen[at.Unix()] = true
		r.Samples = append(r.Samples, BatterySample{At: at, MV: s.BatteryMV})
	}
	sort.Slice(r.Samples, func(i, j int) bool { return r.Samples[i].At.Before(r.Samples[j].At) })
	sort.Slice(r.Temps, func(i, j int) bool { return r.Temps[i].At.Before(r.Temps[j].At) })
	return r
}
