// Package config is HopReact's single source of runtime configuration: one
// YAML file, resolved from (in order) an explicit -config flag, the
// HOPREACT_CONFIG environment variable, or the default path "config.yaml".
// Everything else — the CoreScope instance to watch, Discord credentials,
// poll cadence, and the safety rails on alerting — lives in that one file
// rather than being spread across environment variables.
//
// A config file at an explicitly-requested path (flag or env) that doesn't
// exist is a fatal error: the operator pointed somewhere on purpose, and a
// silent fallback would hide a typo. Missing only at the bare default path
// is not — that's the shape of a fresh checkout, so Load reports it and
// carries on with built-in defaults.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// EnvVar is the only environment variable HopReact reads. Everything else
// is in the file it points at.
const EnvVar = "HOPREACT_CONFIG"

// DefaultPath is where Load looks when neither the flag nor EnvVar is set.
const DefaultPath = "config.yaml"

// MinPollInterval floors poll.interval. Polling someone else's public API
// harder than this achieves nothing — the minimum alert threshold is an
// hour — and would just be rude.
const MinPollInterval = time.Minute

// MinThresholdHours is the smallest alert threshold a user may choose, and
// is a product decision rather than a technical limit: below an hour, normal
// mesh quiet periods start producing alerts that aren't real.
const MinThresholdHours = 1

// Config is the full YAML schema. Every field has a built-in default (see
// Default), so a config.yaml only needs the values it actually changes.
type Config struct {
	// DataDir holds the SQLite database. The directory itself must be
	// writable, not just the file: SQLite writes -wal and -shm sidecars
	// alongside it.
	DataDir string `yaml:"data_dir"`

	Site      SiteConfig      `yaml:"site"`
	HTTP      HTTPConfig      `yaml:"http"`
	CoreScope CoreScopeConfig `yaml:"corescope"`
	Discord   DiscordConfig   `yaml:"discord"`
	Poll      PollConfig      `yaml:"poll"`
	Alerts    AlertsConfig    `yaml:"alerts"`
	Status    StatusConfig    `yaml:"status"`
}

// StatusConfig drives the public status board. It needs no sign-in, so
// everything it shows is a deliberate choice rather than a default.
type StatusConfig struct {
	// Enabled publishes /status. Off by default: an operator should decide to
	// put their mesh on a public page, not discover they already have.
	Enabled bool `yaml:"enabled"`
	// GeoJSONPath restricts the board to repeaters inside a region. Empty
	// lists every repeater the feed carries — which on a bridged mesh can be
	// mostly other people's, so a region is usually what you want.
	GeoJSONPath string `yaml:"geojson_path"`
	// RegionName overrides the name found in the GeoJSON.
	RegionName string `yaml:"region_name"`
	// Title heads the page.
	Title string `yaml:"title"`

	// The escalation ladder, in hours of silence. A repeater is judged on the
	// most recent thing seen of it — an advert OR ordinary traffic — so
	// either one arriving puts it back to green.
	//
	// QuietHours is worth a look, ConcernHours is worth asking about, and
	// AlarmHours means it has almost certainly stopped.
	QuietHours   int `yaml:"quiet_hours"`
	ConcernHours int `yaml:"concern_hours"`
	AlarmHours   int `yaml:"alarm_hours"`

	// RecentHours and RecentLimit control the "recently changed" strip.
	RecentHours int `yaml:"recent_hours"`
	RecentLimit int `yaml:"recent_limit"`

	// CoreBridgeScore and CoreTrafficShare mark a repeater as backbone.
	// Either one qualifies, because they measure different things: traffic
	// share is how much of the mesh's traffic actually goes through a node,
	// while bridge score is betweenness centrality — how many shortest paths
	// between other pairs of nodes run through it. A quiet chokepoint scores
	// low on the first and high on the second, and losing it still splits the
	// mesh in two.
	//
	// Defaults follow CoreScope's own labelling, where bridge >= 0.2 reads
	// "Important" and traffic share >= 0.3 reads "Moderate". On the live mesh
	// that picks 14 of 71 Scottish repeaters, which is about the right size
	// for a backbone list.
	CoreBridgeScore  float64 `yaml:"core_bridge_score"`
	CoreTrafficShare float64 `yaml:"core_traffic_share"`

	// Services are the dependencies HopReact cannot infer from packets: the
	// MQTT broker observers report through, and whatever runs beside it.
	Services []ServiceCheck `yaml:"services"`
	// ServiceInterval is how often those are probed. Results are cached, so a
	// public page never becomes a request amplifier.
	ServiceInterval time.Duration `yaml:"service_interval"`
}

// ServiceCheck is one dependency shown on the board.
type ServiceCheck struct {
	Name string `yaml:"name"`
	// Kind is "http" (the default) or "tcp".
	Kind string `yaml:"kind"`
	// Target is a URL for http, or host:port for tcp.
	Target string `yaml:"target"`
	// Note explains what it does, shown on hover.
	Note string `yaml:"note"`
}

type SiteConfig struct {
	// BaseURL is the public origin. It builds the OAuth redirect URI and
	// every link in a Discord message, so it has to match the redirect URI
	// registered with Discord exactly. Whether it's https also decides
	// whether the session cookie is issued Secure — see internal/auth.
	BaseURL string `yaml:"base_url"`
	Name    string `yaml:"name"`
	// PrivacyPolicyURL is linked from the footer and from alerts. Empty
	// hides the link rather than rendering a dead one.
	PrivacyPolicyURL string `yaml:"privacy_policy_url"`
}

type HTTPConfig struct {
	Listen string `yaml:"listen"`
}

type CoreScopeConfig struct {
	// APIURL is the CoreScope instance to watch — public and
	// unauthenticated, the same instance HopReach reads.
	APIURL                string `yaml:"api_url"`
	RequestTimeoutSeconds int    `yaml:"request_timeout_seconds"`
}

type DiscordConfig struct {
	ClientID     string `yaml:"client_id"`
	ClientSecret string `yaml:"client_secret"`
	// BotToken sends the DMs, so unlike a user's OAuth token it has to
	// outlive any session.
	BotToken string `yaml:"bot_token"`
	// GuildID is HopReact's own Discord server. Sign-in adds the user to it
	// (OAuth scope guilds.join) because Discord will not let a bot DM
	// someone it shares no server with — which is also why leaving that
	// server silently disables a user's alerts, and why membership is
	// re-checked rather than assumed.
	GuildID string `yaml:"guild_id"`
	// OperatorUserID receives HopReact's own health messages: failed polls
	// and the alert breaker tripping. Empty means those go only to the log.
	OperatorUserID string `yaml:"operator_user_id"`
}

type PollConfig struct {
	Interval time.Duration `yaml:"interval"`

	// The rest of this struct exists so an upstream failure can never be
	// mistaken for "every node went offline at once". See internal/alert.

	// MinNodes: a poll returning fewer nodes than this is disbelieved
	// rather than acted on. The real feed carries hundreds.
	MinNodes int `yaml:"min_nodes"`
	// MinNodeFraction: ...and likewise below this share of the largest
	// recent poll, which catches partial responses that still clear
	// MinNodes.
	MinNodeFraction float64 `yaml:"min_node_fraction"`
	// MaxPollsWithoutAdvance: a full, well-formed node list can still be
	// frozen if CoreScope's own upstream died. If no node's last_seen
	// advances for this many consecutive polls, treat the data as stale and
	// don't evaluate. A mesh going completely silent looks identical from
	// here, and being wrong in that direction is much cheaper.
	MaxPollsWithoutAdvance int `yaml:"max_polls_without_advance"`

	// PacketLimit is how many packets each poll reads for per-type evidence.
	// The mesh runs about five packets a minute, so the default covers a
	// couple of hours — enough overlap that a few missed polls lose nothing,
	// and one request regardless of how many nodes are being watched.
	PacketLimit int `yaml:"packet_limit"`

	// BackfillHours is how much history to read once, on first run, so the
	// per-type view has something to show immediately rather than reading
	// "never" against everything for its first day. CoreScope does the
	// windowing, so this costs a single request. 0 disables it.
	BackfillHours int `yaml:"backfill_hours"`
}

type AlertsConfig struct {
	// ConfirmPolls is how many consecutive polls a watch must stay over its
	// threshold before it alerts — the guard against a node sitting right
	// on the boundary and emitting a stream of down/up messages.
	ConfirmPolls int `yaml:"confirm_polls"`
	// MaxNewAlertsPerPoll / MaxNewAlertFraction bound how many watches may
	// enter the alerting state in one poll before the breaker trips,
	// freezes the state machine and tells only the operator. Whichever
	// limit is hit first wins. This is the backstop for the upstream
	// failure nobody predicted.
	MaxNewAlertsPerPoll int     `yaml:"max_new_alerts_per_poll"`
	MaxNewAlertFraction float64 `yaml:"max_new_alert_fraction"`
	// MaxWatchesPerUser caps how many watches one account may hold.
	MaxWatchesPerUser int `yaml:"max_watches_per_user"`
}

// Default returns the built-in configuration. config.example.yaml documents
// these same values; keep the two in step.
func Default() Config {
	return Config{
		DataDir: "./data",
		Site: SiteConfig{
			BaseURL:          "http://localhost:8080",
			Name:             "HopReact",
			PrivacyPolicyURL: "",
		},
		HTTP: HTTPConfig{Listen: ":8080"},
		CoreScope: CoreScopeConfig{
			APIURL:                "https://scotmesh-corescope.mm7roq.compute.oarc.uk",
			RequestTimeoutSeconds: 30,
		},
		Poll: PollConfig{
			Interval:               5 * time.Minute,
			MinNodes:               100,
			MinNodeFraction:        0.5,
			MaxPollsWithoutAdvance: 2,
			PacketLimit:            600,
			BackfillHours:          24,
		},
		Status: StatusConfig{
			Enabled:          false,
			Title:            "Repeater status",
			QuietHours:       12,
			ConcernHours:     24,
			AlarmHours:       36,
			RecentHours:      24,
			RecentLimit:      10,
			CoreBridgeScore:  0.2,
			CoreTrafficShare: 0.3,
			ServiceInterval:  time.Minute,
		},
		Alerts: AlertsConfig{
			ConfirmPolls:        2,
			MaxNewAlertsPerPoll: 10,
			MaxNewAlertFraction: 0.25,
			MaxWatchesPerUser:   50,
		},
	}
}

// resolvePath reports the config path and whether it was asked for
// explicitly, which is what lets Load distinguish an operator's typo from a
// fresh checkout.
func resolvePath(flagVal string) (path string, explicit bool) {
	if flagVal != "" {
		return flagVal, true
	}
	if v := os.Getenv(EnvVar); v != "" {
		return v, true
	}
	return DefaultPath, false
}

// Load reads, parses and validates the configuration, returning it along
// with the path it came from (worth logging — "which config is this process
// actually using" is the first question during any incident).
//
// Defaults are the starting point and YAML is unmarshalled on top, so an
// absent key keeps its default rather than becoming a zero value, and there
// is no separate merge step to keep in sync.
func Load(flagVal string) (Config, string, error) {
	path, explicit := resolvePath(flagVal)

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) && !explicit {
			fmt.Printf("config: no config file at %q, using built-in defaults\n", path)
			cfg := Default()
			if err := cfg.Validate(); err != nil {
				return Config{}, path, fmt.Errorf("config: built-in defaults: %w", err)
			}
			return cfg, path, nil
		}
		return Config{}, path, fmt.Errorf("config: reading %s: %w", path, err)
	}

	cfg := Default()
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, path, fmt.Errorf("config: parsing %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, path, fmt.Errorf("config: %s: %w", path, err)
	}
	return cfg, path, nil
}

// Validate catches the mistakes that would otherwise fail confusingly later
// — at the first poll, or worse, at the first attempt to send an alert.
//
// It deliberately does NOT require the Discord credentials: they're empty in
// the shipped defaults, and a run without them is a legitimate way to bring
// the service up and look around before wiring Discord in. Whether Discord
// is usable is reported by DiscordConfigured instead, so the UI can say so
// plainly rather than the process refusing to boot.
func (c Config) Validate() error {
	if strings.TrimSpace(c.DataDir) == "" {
		return fmt.Errorf("data_dir must not be empty")
	}
	if strings.TrimSpace(c.Site.BaseURL) == "" {
		return fmt.Errorf("site.base_url must not be empty")
	}
	if !strings.HasPrefix(c.Site.BaseURL, "http://") && !strings.HasPrefix(c.Site.BaseURL, "https://") {
		return fmt.Errorf("site.base_url must start with http:// or https://, got %q", c.Site.BaseURL)
	}
	if strings.HasSuffix(c.Site.BaseURL, "/") {
		return fmt.Errorf("site.base_url must not end with a trailing slash, got %q", c.Site.BaseURL)
	}
	if strings.TrimSpace(c.HTTP.Listen) == "" {
		return fmt.Errorf("http.listen must not be empty")
	}
	if strings.TrimSpace(c.CoreScope.APIURL) == "" {
		return fmt.Errorf("corescope.api_url must not be empty")
	}
	if c.CoreScope.RequestTimeoutSeconds <= 0 {
		return fmt.Errorf("corescope.request_timeout_seconds must be positive, got %d", c.CoreScope.RequestTimeoutSeconds)
	}
	if c.Poll.Interval < MinPollInterval {
		return fmt.Errorf("poll.interval must be at least %s, got %s", MinPollInterval, c.Poll.Interval)
	}
	if c.Poll.MinNodes < 0 {
		return fmt.Errorf("poll.min_nodes must not be negative, got %d", c.Poll.MinNodes)
	}
	if c.Poll.MinNodeFraction < 0 || c.Poll.MinNodeFraction > 1 {
		return fmt.Errorf("poll.min_node_fraction must be between 0 and 1, got %v", c.Poll.MinNodeFraction)
	}
	if c.Poll.MaxPollsWithoutAdvance < 1 {
		return fmt.Errorf("poll.max_polls_without_advance must be at least 1, got %d", c.Poll.MaxPollsWithoutAdvance)
	}
	if c.Poll.PacketLimit < 0 || c.Poll.PacketLimit > 10000 {
		return fmt.Errorf("poll.packet_limit must be between 0 and 10000, got %d", c.Poll.PacketLimit)
	}
	if c.Poll.BackfillHours < 0 || c.Poll.BackfillHours > 168 {
		return fmt.Errorf("poll.backfill_hours must be between 0 and 168, got %d", c.Poll.BackfillHours)
	}
	if c.Status.Enabled {
		if c.Status.QuietHours < 1 {
			return fmt.Errorf("status.quiet_hours must be at least 1, got %d", c.Status.QuietHours)
		}
		// Each tier has to be reachable, or a colour on the board is dead code.
		if c.Status.ConcernHours <= c.Status.QuietHours {
			return fmt.Errorf("status.concern_hours (%d) must be greater than status.quiet_hours (%d)",
				c.Status.ConcernHours, c.Status.QuietHours)
		}
		if c.Status.AlarmHours <= c.Status.ConcernHours {
			return fmt.Errorf("status.alarm_hours (%d) must be greater than status.concern_hours (%d)",
				c.Status.AlarmHours, c.Status.ConcernHours)
		}
		if c.Status.RecentHours < 1 {
			return fmt.Errorf("status.recent_hours must be at least 1, got %d", c.Status.RecentHours)
		}
		if c.Status.RecentLimit < 0 {
			return fmt.Errorf("status.recent_limit must not be negative")
		}
		if c.Status.ServiceInterval > 0 && c.Status.ServiceInterval < 30*time.Second {
			return fmt.Errorf("status.service_interval must be at least 30s, got %s", c.Status.ServiceInterval)
		}
		for i, sc := range c.Status.Services {
			if sc.Name == "" || sc.Target == "" {
				return fmt.Errorf("status.services[%d] needs a name and a target", i)
			}
			switch sc.Kind {
			case "", "http", "tcp":
			default:
				return fmt.Errorf("status.services[%d].kind must be http or tcp, got %q", i, sc.Kind)
			}
		}
	}
	if c.Alerts.ConfirmPolls < 1 {
		return fmt.Errorf("alerts.confirm_polls must be at least 1, got %d", c.Alerts.ConfirmPolls)
	}
	if c.Alerts.MaxNewAlertsPerPoll < 1 {
		return fmt.Errorf("alerts.max_new_alerts_per_poll must be at least 1, got %d", c.Alerts.MaxNewAlertsPerPoll)
	}
	if c.Alerts.MaxNewAlertFraction <= 0 || c.Alerts.MaxNewAlertFraction > 1 {
		return fmt.Errorf("alerts.max_new_alert_fraction must be between 0 (exclusive) and 1, got %v", c.Alerts.MaxNewAlertFraction)
	}
	if c.Alerts.MaxWatchesPerUser < 1 {
		return fmt.Errorf("alerts.max_watches_per_user must be at least 1, got %d", c.Alerts.MaxWatchesPerUser)
	}
	return nil
}

// DiscordConfigured reports whether there's enough to run the Discord side
// at all. Everything named here is required together: without the guild the
// bot can't be in a server with the user, and without that it cannot DM them
// no matter what else is set.
func (c Config) DiscordConfigured() bool {
	return c.Discord.ClientID != "" &&
		c.Discord.ClientSecret != "" &&
		c.Discord.BotToken != "" &&
		c.Discord.GuildID != ""
}

// RequestTimeout is CoreScope's per-request timeout as a Duration.
func (c Config) RequestTimeout() time.Duration {
	return time.Duration(c.CoreScope.RequestTimeoutSeconds) * time.Second
}
