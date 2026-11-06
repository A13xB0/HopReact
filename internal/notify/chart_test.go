package notify

import (
	"bytes"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

	"hopreact/internal/alert"
)

var london, _ = time.LoadLocation("Europe/London")

// weekOf builds hourly readings: a line moving perDay mV a day with a daytime
// bump of swing mV.
func weekOf(end time.Time, startMV, perDay, swing float64) []alert.Sample {
	var out []alert.Sample
	for i := 0; i <= 7*24; i++ {
		at := end.Add(-time.Duration(7*24-i) * time.Hour)
		mv := startMV + perDay*float64(i)/24
		if h := at.In(london).Hour(); h >= 9 && h <= 16 {
			mv += swing
		}
		out = append(out, alert.Sample{At: at, MV: int(mv)})
	}
	return out
}

func TestBatteryChartDraws(t *testing.T) {
	now := time.Date(2026, 11, 6, 14, 20, 0, 0, time.UTC)
	lv := alert.BatteryLevels{LowMV: 3650, CriticalMV: 3500}
	cases := map[string][]alert.Sample{
		"draining": weekOf(now, 3950, -45, 25),
		"steady":   weekOf(now, 4050, 0, 60),
		"charging": weekOf(now, 3780, 30, 30),
	}
	for name, s := range cases {
		tr := alert.AnalyseTrend(s, now, london)
		img, err := BatteryChart(ChartInput{Name: "Bowden_R63", Samples: s, Now: now, Trend: tr, Levels: lv, Loc: london})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		cfg, err := png.DecodeConfig(bytes.NewReader(img))
		if err != nil || cfg.Width != chartW || cfg.Height != chartH {
			t.Fatalf("%s: not a %dx%d PNG: %v", name, chartW, chartH, err)
		}
		// HOPREACT_CHART_DIR=/some/dir go test ./internal/notify -run Chart
		// writes the charts out for a look.
		if dir := os.Getenv("HOPREACT_CHART_DIR"); dir != "" {
			if err := os.WriteFile(filepath.Join(dir, name+".png"), img, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestBatteryChartNeedsReadings(t *testing.T) {
	if _, err := BatteryChart(ChartInput{Now: time.Now()}); err == nil {
		t.Fatal("drew a chart with no readings")
	}
}

func TestChartTitleDropsEmoji(t *testing.T) {
	if err := loadFonts(); err != nil {
		t.Fatal(err)
	}
	if got := drawable("West Lomond ⛰️🌤", faceBold); got != "West Lomond" {
		t.Errorf("drawable = %q", got)
	}
	if got := drawable("🏘️", faceBold); got != "Repeater" {
		t.Errorf("all-emoji name = %q", got)
	}
}
