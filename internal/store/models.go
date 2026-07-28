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
	CreatedAt      time.Time
}

// Signal is which liveness question a piece of state answers.
type Signal string

const (
	// SignalSeen is "heard at all" — the default, and the only signal that
	// applies to observers.
	SignalSeen Signal = "seen"
	// SignalRelayed is "still appearing in packet routes" — opt-in per
	// watch.
	SignalRelayed Signal = "relayed"
)

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

// WatchState is the alert state of one watch for one signal.
type WatchState struct {
	WatchID       int64
	Signal        Signal
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

// WatchView is a watch joined to its target and state, which is what the
// dashboard renders.
type WatchView struct {
	Watch  Watch
	Target *Target // nil when CoreScope has never reported this key
	Seen   WatchState
	Relay  *WatchState // nil unless the watch opted into the relay signal
}
