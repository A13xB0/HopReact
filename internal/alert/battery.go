package alert

import "time"

// BatteryLevel is where a battery rule stands.
type BatteryLevel string

const (
	// BatteryUnknown means there is nothing to judge: no reading yet, or a
	// node reporting 0 mV, which is a node with no battery fitted.
	BatteryUnknown     BatteryLevel = "unknown"
	BatteryOK          BatteryLevel = "ok"
	BatteryLow         BatteryLevel = "low"
	BatteryCritical    BatteryLevel = "critical"
	BatteryOvercharged BatteryLevel = "overcharged"
)

// down reports whether a level is a low-battery level.
func (l BatteryLevel) down() bool { return l == BatteryLow || l == BatteryCritical }

// BatteryLevels are the voltages one rule judges against, already resolved
// from the watch's own settings, the site's and CoreScope's.
type BatteryLevels struct {
	LowMV      int
	CriticalMV int
	// OverchargeMV is 0 when the overcharge alert is off for this watch.
	OverchargeMV int
}

// BatteryReading is the newest reading CoreScope has for a node.
type BatteryReading struct {
	MV int
	At time.Time
	// HasBattery is false for a node reporting 0 mV.
	HasBattery bool
}

// BatteryState is a battery rule's stored state.
type BatteryState struct {
	Level BatteryLevel
	Since time.Time
	// Candidate is the level the recent readings point to, and
	// CandidateCount how many new readings in a row have agreed.
	Candidate      BatteryLevel
	CandidateCount int
	LastReadingAt  time.Time
	LastMV         int
	// Announced is the most serious level anyone was told about in this
	// episode. Empty means nobody was told, which also silences the "OK
	// again" message: you never announce recovery from an alert that was
	// never sent.
	Announced BatteryLevel
}

// BatteryPolicy is the site's tuning.
type BatteryPolicy struct {
	ConfirmReadings int
	RecoverMarginMV int
}

// BatteryOutcome is the result of one evaluation.
type BatteryOutcome struct {
	State BatteryState
	// Changed means State differs from what was stored and must be saved.
	Changed bool
	// Notify is set when a message should go out, and Kind says which:
	// "low", "critical", "overcharged" or "recovered".
	Notify bool
	Kind   string
	// From is the level before this transition.
	From BatteryLevel
}

// ClassifyBattery says which level a reading belongs to, given the level the
// rule is already at. The margin is hysteresis: once low, a battery has to
// climb back above low + margin to count as OK, so a cell sitting on the line
// cannot send a message on every reading.
func ClassifyBattery(mv int, cur BatteryLevel, lv BatteryLevels, marginMV int) BatteryLevel {
	if lv.OverchargeMV > 0 {
		if mv >= lv.OverchargeMV {
			return BatteryOvercharged
		}
		if cur == BatteryOvercharged && mv > lv.OverchargeMV-marginMV {
			return BatteryOvercharged
		}
	}
	switch {
	case mv < lv.CriticalMV:
		return BatteryCritical
	case cur == BatteryCritical && mv < lv.CriticalMV+marginMV:
		return BatteryCritical
	case mv < lv.LowMV:
		return BatteryLow
	case cur.down() && mv < lv.LowMV+marginMV:
		return BatteryLow
	}
	return BatteryOK
}

// sameRun reports whether two levels are the same direction of change, so a
// battery dropping through low into critical in successive readings confirms
// as one run rather than restarting the count at each level.
func sameRun(a, b BatteryLevel) bool {
	return a == b || (a.down() && b.down())
}

// EvaluateBattery advances one battery rule by one fetched reading.
//
// Only a NEW reading can move anything: CoreScope is fetched more often than
// readings arrive, and counting the same reading twice would let one odd
// value confirm itself.
func EvaluateBattery(r BatteryReading, cur BatteryState, lv BatteryLevels, pol BatteryPolicy, prefs Prefs) BatteryOutcome {
	confirm := pol.ConfirmReadings
	if confirm < 1 {
		confirm = 1
	}
	if cur.Level == "" {
		cur.Level = BatteryUnknown
	}
	next := cur
	out := BatteryOutcome{From: cur.Level}

	if !r.HasBattery {
		// 0 mV: no battery fitted, so there is nothing to judge. Dropping to
		// unknown is silent, and forgets the episode, because a node that
		// reports no battery has not recovered from anything.
		if cur.Level != BatteryUnknown || cur.Candidate != "" || cur.Announced != "" {
			next.Level, next.Candidate, next.CandidateCount, next.Announced = BatteryUnknown, "", 0, ""
			if !r.At.IsZero() {
				next.Since = r.At
			}
			out.State, out.Changed = next, true
			return out
		}
		out.State = next
		return out
	}
	if r.At.IsZero() || !r.At.After(cur.LastReadingAt) {
		out.State = next
		return out
	}

	next.LastReadingAt, next.LastMV = r.At, r.MV
	out.Changed = true
	target := ClassifyBattery(r.MV, cur.Level, lv, pol.RecoverMarginMV)

	if cur.Level == BatteryUnknown {
		// First real reading. Adopt it without announcing anything, the same
		// way a new watch on a node that is already down stays quiet.
		next.Level, next.Since, next.Candidate, next.CandidateCount = target, r.At, "", 0
		out.State = next
		return out
	}
	if target == cur.Level {
		next.Candidate, next.CandidateCount = "", 0
		out.State = next
		return out
	}
	if cur.Candidate != "" && sameRun(cur.Candidate, target) {
		next.CandidateCount = cur.CandidateCount + 1
	} else {
		next.CandidateCount = 1
	}
	next.Candidate = target
	if next.CandidateCount < confirm {
		out.State = next
		return out
	}

	prev := cur.Level
	next.Level, next.Since, next.Candidate, next.CandidateCount = target, r.At, "", 0

	kind := ""
	switch target {
	case BatteryLow:
		// Climbing out of critical into low is good news, not a new alert.
		if prev != BatteryCritical {
			kind = "low"
		}
	case BatteryCritical:
		kind = "critical"
	case BatteryOvercharged:
		kind = "overcharged"
	case BatteryOK:
		if cur.Announced != "" && prefs.NotifyRecovery {
			kind = "recovered"
		}
		next.Announced = ""
	}
	if kind != "" && !prefs.Muted {
		out.Notify, out.Kind = true, kind
		if kind != "recovered" {
			next.Announced = target
		}
	}
	out.State = next
	return out
}

// Fallback levels when neither the watch, the site nor CoreScope set one.
// They are CoreScope's own defaults.
const (
	DefaultLowMV      = 3300
	DefaultCriticalMV = 3000
)

// ResolveBatteryLevels picks the levels one rule judges against: the watch's
// own, else the site's, else CoreScope's. overMV is only used when the
// overcharge alert is on.
func ResolveBatteryLevels(ruleLow, ruleCrit int, overOn bool, ruleOver int,
	siteLow, siteCrit, siteOver, scopeLow, scopeCrit int) BatteryLevels {
	pick := func(vals ...int) int {
		for _, v := range vals {
			if v > 0 {
				return v
			}
		}
		return 0
	}
	lv := BatteryLevels{
		LowMV:      pick(ruleLow, siteLow, scopeLow, DefaultLowMV),
		CriticalMV: pick(ruleCrit, siteCrit, scopeCrit, DefaultCriticalMV),
	}
	if overOn {
		lv.OverchargeMV = pick(ruleOver, siteOver)
	}
	return lv
}
