package alert

import (
	"testing"
	"time"
)

var (
	bt0   = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	lvls  = BatteryLevels{LowMV: 3650, CriticalMV: 3500}
	bpol  = BatteryPolicy{ConfirmReadings: 2, RecoverMarginMV: 100}
	loud  = Prefs{NotifyRecovery: true}
	quiet = Prefs{Muted: true, NotifyRecovery: true}
)

// feed runs readings through the evaluator in order and returns every
// outcome.
func feed(st BatteryState, lv BatteryLevels, prefs Prefs, mvs ...int) (BatteryState, []BatteryOutcome) {
	var outs []BatteryOutcome
	base := bt0
	if st.LastReadingAt.After(base) {
		base = st.LastReadingAt
	}
	for i, mv := range mvs {
		out := EvaluateBattery(BatteryReading{MV: mv, At: base.Add(time.Duration(i+1) * time.Hour), HasBattery: true},
			st, lv, bpol, prefs)
		outs = append(outs, out)
		st = out.State
	}
	return st, outs
}

func kinds(outs []BatteryOutcome) []string {
	var k []string
	for _, o := range outs {
		if o.Notify {
			k = append(k, o.Kind)
		}
	}
	return k
}

func eqStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestBatteryFirstReadingIsAdoptedSilently(t *testing.T) {
	st, outs := feed(BatteryState{}, lvls, loud, 3400)
	if st.Level != BatteryCritical || len(kinds(outs)) != 0 {
		t.Fatalf("level %s, messages %v: a battery already flat when we start looking must not announce itself", st.Level, kinds(outs))
	}
}

func TestBatteryLowNeedsTwoReadings(t *testing.T) {
	st, outs := feed(BatteryState{}, lvls, loud, 3900, 3640)
	if st.Level != BatteryOK || len(kinds(outs)) != 0 {
		t.Fatalf("one low reading changed the level to %s", st.Level)
	}
	st, outs = feed(st, lvls, loud, 3630)
	if st.Level != BatteryLow || !eqStrings(kinds(outs), []string{"low"}) {
		t.Fatalf("level %s, messages %v, want low announced on the second reading", st.Level, kinds(outs))
	}
	if st.Announced != BatteryLow {
		t.Errorf("Announced = %q", st.Announced)
	}
}

func TestBatteryOneOddReadingResetsTheCount(t *testing.T) {
	st, outs := feed(BatteryState{}, lvls, loud, 3900, 3600, 3900, 3600)
	if st.Level != BatteryOK || len(kinds(outs)) != 0 {
		t.Fatalf("alternating readings confirmed %s", st.Level)
	}
}

func TestBatterySameReadingTwiceCountsOnce(t *testing.T) {
	st, _ := feed(BatteryState{}, lvls, loud, 3900, 3600)
	again := BatteryReading{MV: 3600, At: st.LastReadingAt, HasBattery: true}
	out := EvaluateBattery(again, st, lvls, bpol, loud)
	if out.Changed || out.State.Level != BatteryOK {
		t.Fatalf("re-reading the same reading moved the state: %+v", out)
	}
}

func TestBatteryDropsThroughLowToCriticalAsOneRun(t *testing.T) {
	st, outs := feed(BatteryState{}, lvls, loud, 3900, 3600, 3450)
	if st.Level != BatteryCritical || !eqStrings(kinds(outs), []string{"critical"}) {
		t.Fatalf("level %s, messages %v: low then critical should confirm as one fall", st.Level, kinds(outs))
	}
}

func TestBatteryEscalatesThenRecoversWithMargin(t *testing.T) {
	st, outs := feed(BatteryState{}, lvls, loud, 3900, 3600, 3600, 3450, 3450)
	if !eqStrings(kinds(outs), []string{"low", "critical"}) {
		t.Fatalf("messages %v, want low then critical", kinds(outs))
	}
	// Back above critical but inside the margin: still critical.
	st, outs = feed(st, lvls, loud, 3560, 3560)
	if st.Level != BatteryCritical || len(kinds(outs)) != 0 {
		t.Fatalf("level %s inside the margin", st.Level)
	}
	// Into low: quietly better, no new alert.
	st, outs = feed(st, lvls, loud, 3620, 3620)
	if st.Level != BatteryLow || len(kinds(outs)) != 0 {
		t.Fatalf("level %s, messages %v: climbing out of critical is not news", st.Level, kinds(outs))
	}
	// Above low but inside the margin: still low.
	st, _ = feed(st, lvls, loud, 3700, 3700)
	if st.Level != BatteryLow {
		t.Fatalf("level %s at 3.70 V, want low until 3.75 V", st.Level)
	}
	st, outs = feed(st, lvls, loud, 3760, 3760)
	if st.Level != BatteryOK || !eqStrings(kinds(outs), []string{"recovered"}) || st.Announced != "" {
		t.Fatalf("level %s, messages %v, announced %q", st.Level, kinds(outs), st.Announced)
	}
}

func TestBatteryRecoveryRespectsPreference(t *testing.T) {
	noRecovery := Prefs{}
	st, _ := feed(BatteryState{}, lvls, noRecovery, 3900, 3600, 3600)
	_, outs := feed(st, lvls, noRecovery, 3900, 3900)
	if len(kinds(outs)) != 0 {
		t.Fatalf("messages %v with recovery messages off", kinds(outs))
	}
}

func TestBatteryMutedMovesButStaysQuiet(t *testing.T) {
	st, outs := feed(BatteryState{}, lvls, quiet, 3900, 3600, 3600)
	if st.Level != BatteryLow || len(kinds(outs)) != 0 || st.Announced != "" {
		t.Fatalf("muted: level %s, messages %v, announced %q", st.Level, kinds(outs), st.Announced)
	}
	// Nobody was told, so recovery is silent even after unmuting.
	_, outs = feed(st, lvls, loud, 3900, 3900)
	if len(kinds(outs)) != 0 {
		t.Fatalf("recovery announced for an alert nobody heard: %v", kinds(outs))
	}
}

func TestBatteryNoBatteryGoesUnknownSilently(t *testing.T) {
	st, _ := feed(BatteryState{}, lvls, loud, 3900, 3600, 3600)
	out := EvaluateBattery(BatteryReading{At: bt0.Add(10 * time.Hour)}, st, lvls, bpol, loud)
	if out.State.Level != BatteryUnknown || out.Notify || !out.Changed || out.State.Announced != "" {
		t.Fatalf("0 mV: %+v", out)
	}
}

func TestBatteryOverchargeOnlyWhenTurnedOn(t *testing.T) {
	st, outs := feed(BatteryState{}, lvls, loud, 4100, 4300, 4300)
	if st.Level != BatteryOK || len(kinds(outs)) != 0 {
		t.Fatalf("overcharge fired while off: %s %v", st.Level, kinds(outs))
	}
	on := lvls
	on.OverchargeMV = 4250
	st, outs = feed(BatteryState{}, on, loud, 4100, 4300, 4300)
	if st.Level != BatteryOvercharged || !eqStrings(kinds(outs), []string{"overcharged"}) {
		t.Fatalf("level %s, messages %v", st.Level, kinds(outs))
	}
	// 4.20 V is inside the margin below 4.25 V: still overcharged.
	st, _ = feed(st, on, loud, 4200, 4200)
	if st.Level != BatteryOvercharged {
		t.Fatalf("cleared inside the margin: %s", st.Level)
	}
	st, outs = feed(st, on, loud, 4140, 4140)
	if st.Level != BatteryOK || !eqStrings(kinds(outs), []string{"recovered"}) {
		t.Fatalf("level %s, messages %v", st.Level, kinds(outs))
	}
}

func TestClassifyBatteryOwnLevels(t *testing.T) {
	// A LiFePO4 owner's levels: healthy at 3.30 V.
	lfp := BatteryLevels{LowMV: 3100, CriticalMV: 3000}
	if got := ClassifyBattery(3300, BatteryOK, lfp, 100); got != BatteryOK {
		t.Errorf("3.30 V with LiFePO4 levels = %s", got)
	}
	if got := ClassifyBattery(3050, BatteryOK, lfp, 100); got != BatteryLow {
		t.Errorf("3.05 V with LiFePO4 levels = %s", got)
	}
}
