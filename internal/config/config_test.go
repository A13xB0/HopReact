package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDefaultIsValid(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Fatalf("Default() must validate, got %v", err)
	}
}

// A config file only needs the keys it changes — everything absent has to
// keep its default rather than becoming a zero value. This is the whole
// reason Load unmarshals on top of Default() instead of into an empty
// struct, and it's silent when it breaks: the service would come up with a
// zero poll interval and a zero watch cap.
func TestLoadKeepsDefaultsForAbsentKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("site:\n  name: Test Instance\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, gotPath, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if gotPath != path {
		t.Errorf("Load returned path %q, want %q", gotPath, path)
	}
	if cfg.Site.Name != "Test Instance" {
		t.Errorf("Site.Name = %q, want the value from the file", cfg.Site.Name)
	}
	if want := Default().Poll.Interval; cfg.Poll.Interval != want {
		t.Errorf("Poll.Interval = %v, want the default %v", cfg.Poll.Interval, want)
	}
	if want := Default().Alerts.MaxWatchesPerUser; cfg.Alerts.MaxWatchesPerUser != want {
		t.Errorf("Alerts.MaxWatchesPerUser = %d, want the default %d", cfg.Alerts.MaxWatchesPerUser, want)
	}
	if want := Default().Site.BaseURL; cfg.Site.BaseURL != want {
		t.Errorf("Site.BaseURL = %q, want the default %q", cfg.Site.BaseURL, want)
	}
}

// A path the operator asked for explicitly and that doesn't exist is a
// typo, and must fail loudly. The same absence at the bare default path is
// just a fresh checkout, and must not.
func TestMissingFileIsFatalOnlyWhenExplicit(t *testing.T) {
	t.Run("explicit flag", func(t *testing.T) {
		if _, _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
			t.Fatal("expected an error for an explicitly-requested missing file")
		}
	})

	t.Run("explicit env var", func(t *testing.T) {
		t.Setenv(EnvVar, filepath.Join(t.TempDir(), "nope.yaml"))
		if _, _, err := Load(""); err == nil {
			t.Fatal("expected an error for a missing file named by " + EnvVar)
		}
	})

	t.Run("bare default", func(t *testing.T) {
		// Run from a directory with no config.yaml in it. Done by hand
		// rather than with t.Chdir, which needs Go 1.24 — this module
		// targets 1.23, and the mismatch only shows up in CI.
		chdir(t, t.TempDir())
		t.Setenv(EnvVar, "")
		cfg, path, err := Load("")
		if err != nil {
			t.Fatalf("a missing file at the default path must not be an error, got %v", err)
		}
		if path != DefaultPath {
			t.Errorf("path = %q, want %q", path, DefaultPath)
		}
		if cfg.Poll.Interval != Default().Poll.Interval {
			t.Error("expected built-in defaults")
		}
	})
}

func TestResolvePathPrecedence(t *testing.T) {
	t.Setenv(EnvVar, "/from/env.yaml")

	if got, explicit := resolvePath("/from/flag.yaml"); got != "/from/flag.yaml" || !explicit {
		t.Errorf("flag should win: got (%q, %v)", got, explicit)
	}
	if got, explicit := resolvePath(""); got != "/from/env.yaml" || !explicit {
		t.Errorf("env should be used when no flag: got (%q, %v)", got, explicit)
	}

	t.Setenv(EnvVar, "")
	if got, explicit := resolvePath(""); got != DefaultPath || explicit {
		t.Errorf("bare default expected: got (%q, %v)", got, explicit)
	}
}

func TestValidateRejectsBadValues(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{"empty data_dir", func(c *Config) { c.DataDir = "" }},
		{"empty base_url", func(c *Config) { c.Site.BaseURL = "" }},
		{"base_url without scheme", func(c *Config) { c.Site.BaseURL = "example.com" }},
		// A trailing slash would produce "https://x//callback" in the OAuth
		// redirect URI, which Discord compares byte-for-byte against the
		// registered value — a genuinely baffling failure to debug.
		{"base_url with trailing slash", func(c *Config) { c.Site.BaseURL = "https://example.com/" }},
		{"empty listen", func(c *Config) { c.HTTP.Listen = "" }},
		{"empty corescope url", func(c *Config) { c.CoreScope.APIURL = "" }},
		{"zero request timeout", func(c *Config) { c.CoreScope.RequestTimeoutSeconds = 0 }},
		{"poll interval below the floor", func(c *Config) { c.Poll.Interval = 10 * time.Second }},
		{"negative min_nodes", func(c *Config) { c.Poll.MinNodes = -1 }},
		{"min_node_fraction above 1", func(c *Config) { c.Poll.MinNodeFraction = 1.5 }},
		{"zero max_polls_without_advance", func(c *Config) { c.Poll.MaxPollsWithoutAdvance = 0 }},
		{"zero confirm_polls", func(c *Config) { c.Alerts.ConfirmPolls = 0 }},
		{"zero max_new_alerts_per_poll", func(c *Config) { c.Alerts.MaxNewAlertsPerPoll = 0 }},
		{"zero max_new_alert_fraction", func(c *Config) { c.Alerts.MaxNewAlertFraction = 0 }},
		{"zero max_watches_per_user", func(c *Config) { c.Alerts.MaxWatchesPerUser = 0 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default()
			tt.mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Errorf("Validate() accepted %s", tt.name)
			}
		})
	}
}

// The service must be able to start without Discord wired up — that's how
// you bring it up and look around before creating the application. So the
// credentials are reported as missing rather than refused at boot.
func TestDiscordConfiguredNeedsEveryCredential(t *testing.T) {
	cfg := Default()
	if cfg.DiscordConfigured() {
		t.Error("defaults ship without Discord credentials, so this must be false")
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("missing Discord credentials must not fail Validate, got %v", err)
	}

	full := DiscordConfig{ClientID: "a", ClientSecret: "b", BotToken: "c", GuildID: "d"}
	cfg.Discord = full
	if !cfg.DiscordConfigured() {
		t.Error("a fully-populated Discord config should report configured")
	}

	// Each field on its own is load-bearing — in particular GuildID, without
	// which the bot shares no server with the user and cannot DM at all.
	for _, drop := range []string{"ClientID", "ClientSecret", "BotToken", "GuildID"} {
		cfg.Discord = full
		switch drop {
		case "ClientID":
			cfg.Discord.ClientID = ""
		case "ClientSecret":
			cfg.Discord.ClientSecret = ""
		case "BotToken":
			cfg.Discord.BotToken = ""
		case "GuildID":
			cfg.Discord.GuildID = ""
		}
		if cfg.DiscordConfigured() {
			t.Errorf("DiscordConfigured() should be false with %s missing", drop)
		}
	}
}

// The shipped example must stay loadable and in step with Default() —
// otherwise the documented reference drifts from the code and the first
// thing a new operator copies is already wrong.
func TestExampleConfigLoadsAndMatchesDefaults(t *testing.T) {
	path := filepath.Join("..", "..", "config.example.yaml")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("no config.example.yaml alongside: %v", err)
	}
	cfg, _, err := Load(path)
	if err != nil {
		t.Fatalf("config.example.yaml must load cleanly: %v", err)
	}

	def := Default()
	// Compared field by field rather than with reflect.DeepEqual so a
	// failure names the key that drifted.
	if cfg.DataDir != def.DataDir {
		t.Errorf("data_dir: example %q, Default() %q", cfg.DataDir, def.DataDir)
	}
	if cfg.Site.BaseURL != def.Site.BaseURL {
		t.Errorf("site.base_url: example %q, Default() %q", cfg.Site.BaseURL, def.Site.BaseURL)
	}
	if cfg.HTTP.Listen != def.HTTP.Listen {
		t.Errorf("http.listen: example %q, Default() %q", cfg.HTTP.Listen, def.HTTP.Listen)
	}
	if cfg.CoreScope.APIURL != def.CoreScope.APIURL {
		t.Errorf("corescope.api_url: example %q, Default() %q", cfg.CoreScope.APIURL, def.CoreScope.APIURL)
	}
	if cfg.Poll.Interval != def.Poll.Interval {
		t.Errorf("poll.interval: example %v, Default() %v", cfg.Poll.Interval, def.Poll.Interval)
	}
	if cfg.Poll.MinNodes != def.Poll.MinNodes {
		t.Errorf("poll.min_nodes: example %d, Default() %d", cfg.Poll.MinNodes, def.Poll.MinNodes)
	}
	if cfg.Alerts.ConfirmPolls != def.Alerts.ConfirmPolls {
		t.Errorf("alerts.confirm_polls: example %d, Default() %d", cfg.Alerts.ConfirmPolls, def.Alerts.ConfirmPolls)
	}
	if cfg.Alerts.MaxWatchesPerUser != def.Alerts.MaxWatchesPerUser {
		t.Errorf("alerts.max_watches_per_user: example %d, Default() %d", cfg.Alerts.MaxWatchesPerUser, def.Alerts.MaxWatchesPerUser)
	}
}

// chdir switches to dir for the duration of the test and back afterwards.
// Equivalent to testing.T.Chdir, which is only available from Go 1.24; this
// module targets 1.23 so that the version in go.mod, the Dockerfile and CI
// all agree.
func chdir(t *testing.T, dir string) {
	t.Helper()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(prev); err != nil {
			t.Fatal(err)
		}
	})
}
