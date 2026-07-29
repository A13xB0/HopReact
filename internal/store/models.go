package store

import "time"

// User is an account. Deliberately thin: Discord provides both identity and
// delivery, so there is no email, no password hash and nothing here that
// would count as personal data beyond an account id and a display name.
type User struct {
	ID             int64
	DiscordID      string
	Username       string
	Avatar         string
	CreatedAt      time.Time
	LastLoginAt    time.Time
	DMOK           bool
	DMFailedReason string
	DMCheckedAt    time.Time
}

// Target is the last thing CoreScope told us about one watchable node or
// observer.
type Target struct {
	Kind string
	Key  string
	Name string
	Role string

	LastSeen    time.Time
	LastRelayed time.Time
	// LastPacket is when an observer last HEARD something, as opposed to
	// LastSeen, which for an observer is when it last checked in. Zero for
	// nodes.
	LastPacket time.Time
	// RelayEverObserved is zero until this target has been seen relaying at
	// least once. While it is zero the relay signal stays unknown and can
	// never alert — 189 nodes on the live instance have never relayed.
	RelayEverObserved time.Time

	RelayCount1h  int
	RelayCount24h int
	Lat, Lon      *float64

	FirstIndexedAt time.Time
	LastInFeedAt   time.Time
	UpdatedAt      time.Time
}

// Watch is one person's subscription to one target. Not ownership: several
// users may watch the same target with different thresholds.
type Watch struct {
	ID             int64
	UserID         int64
	TargetKind     string
	TargetKey      string
	ThresholdHours int
	AlertOnRelay   bool
	Label          string
	MutedUntil     time.Time
	// NotifyRecovery controls the "back online" message. On by default: you
	// are told when something you were warned about is fixed.
	NotifyRecovery bool
	CreatedAt      time.Time
}

// Source is where a rule's timestamp comes from.
type Source string

const (
	// SourceSeen is CoreScope's own last_seen — heard at all. The only source
	// that applies to observers.
	SourceSeen Source = "seen"
	// SourceRelayed is CoreScope's own last_relayed — seen in any packet
	// route, at whatever hop width.
	SourceRelayed Source = "relayed"
	// SourcePackets is when an observer last heard traffic, as opposed to
	// when it last checked in. Only meaningful for observers, and a much
	// noisier signal than SourceSeen — a quiet mesh silences it without
	// anything being wrong.
	SourcePackets Source = "packets"
	// SourceTypes reads the per-type evidence HopReact attributes itself.
	//
	// Strictly narrower than the two above: it only counts adverts (whose
	// sender is stated outright) and path hops at least three bytes wide,
	// which is roughly 41% of packets. That is the price of never guessing
	// who a hop belonged to — and the reason a rule with no evidence must
	// stay unknown rather than being read as silence.
	SourceTypes Source = "types"
)

// Direction is how a node was involved in a packet. Mirrors
// attribute.Direction; duplicated here so the store does not depend on the
// attribution package.
type Direction string

const (
	DirSent    Direction = "sent"
	DirCarried Direction = "carried"
	// DirEither accepts evidence of either kind. Only valid on a rule, never
	// on a stored activity row.
	DirEither Direction = "either"
)

// Rule is one alerting condition on a watch. A watch may have several, and
// they are ORed: any one going over its threshold raises the alert.
type Rule struct {
	ID      int64
	WatchID int64
	Label   string
	Source  Source
	// Types are payload type integers, for SourceTypes. Stored expanded
	// rather than as a group name, so redefining a group later cannot change
	// what an existing rule alerts on.
	Types          []int
	Direction      Direction
	ThresholdHours int
	CreatedAt      time.Time
}

// Activity is when one payload type was last observed for one target, in one
// direction.
type Activity struct {
	PayloadType   int
	Direction     Direction
	LastAt        time.Time
	EvidenceCount int
}

// State is a point in the alert lifecycle.
type State string

const (
	// StateUnknown means there is nothing to judge yet: the target has
	// never been in the feed, or the signal has never had a value.
	StateUnknown State = "unknown"
	StateOK      State = "ok"
	// StatePending is over the threshold but not yet confirmed — the flap
	// guard.
	StatePending  State = "pending"
	StateAlerting State = "alerting"
	// StateRecovering is fresh again but not yet confirmed.
	StateRecovering State = "recovering"
)

// WatchState is the alert state of one watch for one rule.
type WatchState struct {
	WatchID       int64
	RuleID        int64
	State         State
	Since         time.Time
	Consecutive   int
	ObservedAt    time.Time
	AlertingSince time.Time
	LastNotified  time.Time
	// NotifyCount is how many alerts have been announced for the current
	// episode. Zero suppresses the recovery message, so a watch seeded onto
	// an already-offline target never announces a recovery for an alert
	// nobody was told about.
	NotifyCount int
	Seeded      bool
}

// PollStatus is whether a poll's data was believed.
type PollStatus string

const (
	PollOK PollStatus = "ok"
	// PollFailed is an upstream error: nothing was fetched.
	PollFailed PollStatus = "failed"
	// PollSuspect is data that arrived but failed a plausibility check —
	// too few nodes, or a feed that has stopped advancing. Never evaluated.
	PollSuspect PollStatus = "suspect"
)

// PollRun records one poll attempt.
type PollRun struct {
	ID               int64
	StartedAt        time.Time
	FinishedAt       time.Time
	Status           PollStatus
	NodeCount        int
	ObserverCount    int
	AdvancedCount    int
	Evaluated        bool
	SuppressedReason string
	Error            string
}

// Notification is one queued outbox message.
type Notification struct {
	ID        int64
	UserID    int64
	DiscordID string
	Kind      string
	Payload   string
	CreatedAt time.Time
	SendAfter time.Time
	Attempts  int
	SentAt    time.Time
	LastError string
}

// RuleView is a rule with its current alert state, for rendering.
type RuleView struct {
	Rule  Rule
	State WatchState
}

// WatchView is a watch joined to its target and every rule's state, which is
// what the dashboard renders.
type WatchView struct {
	Watch  Watch
	Target *Target // nil when CoreScope has never reported this key
	Rules  []RuleView
	// AlsoNode is set on an observer watch whose key is also a mesh node —
	// the same physical box doing both jobs, which is 7 of the 9 observers on
	// the live instance. Such a box does appear in packet routes, so per-type
	// rules mean something for it.
	AlsoNode bool
}

// Worst returns the rule in the most serious state, which is what the summary
// row shows. Alerting beats pending beats everything else; among equals the
// one that has been that way longest wins.
func (v WatchView) Worst() *RuleView {
	rank := map[State]int{
		StateAlerting: 4, StatePending: 3, StateRecovering: 2,
		StateOK: 1, StateUnknown: 0,
	}
	var best *RuleView
	for i := range v.Rules {
		r := &v.Rules[i]
		if best == nil || rank[r.State.State] > rank[best.State.State] ||
			(rank[r.State.State] == rank[best.State.State] && r.State.Since.Before(best.State.Since)) {
			best = r
		}
	}
	return best
}
