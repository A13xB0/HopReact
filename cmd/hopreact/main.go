// Command hopreact watches a CoreScope instance and sends a Discord DM when
// a repeater or observer someone has claimed stops being seen.
//
// One process does everything: an HTTP server for sign-in and the dashboard,
// a poller that fetches CoreScope and evaluates every watch against it, and a
// drainer that turns the resulting decisions into DMs. They share one SQLite
// database, which is also why exactly one instance may run at a time — see
// the lock below.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"hopreact/internal/buildinfo"
	"hopreact/internal/config"
	"hopreact/internal/corescope"
	"hopreact/internal/discord"
	"hopreact/internal/geo"
	"hopreact/internal/health"
	"hopreact/internal/notify"
	"hopreact/internal/poller"
	"hopreact/internal/store"
	"hopreact/internal/web"
)

// drainInterval is how often the outbox is swept. Much shorter than the poll
// interval so an alert produced by a poll goes out promptly rather than
// waiting for the next one.
const drainInterval = 20 * time.Second

// membershipInterval is how often we re-check that users are still in the
// alert server. Someone who quietly left has alerts that will never arrive,
// and finding that out when their repeater dies is the wrong time.
const membershipInterval = 6 * time.Hour

func main() {
	configFlag := flag.String("config", "", "path to config.yaml (default: $HOPREACT_CONFIG, then ./config.yaml)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(buildinfo.Version)
		return
	}

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, path, err := config.Load(*configFlag)
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}
	log.Info("hopreact starting", "version", buildinfo.Version, "config", path)

	st, err := store.Open(cfg.DataDir, time.Now)
	if err != nil {
		log.Error("opening database", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	// SQLite plus an in-process ticker means exactly one instance. Two
	// replicas would double every alert, which is the sort of thing nobody
	// notices until users complain.
	unlock, err := lockDataDir(cfg.DataDir)
	if err != nil {
		log.Error("another hopreact appears to be running against this data directory", "err", err, "dir", cfg.DataDir)
		os.Exit(1)
	}
	defer unlock()

	scope := corescope.NewClient(cfg.CoreScope.APIURL, cfg.RequestTimeout(), nil)
	dc := discord.New(cfg.Discord.ClientID, cfg.Discord.ClientSecret,
		cfg.Discord.BotToken, cfg.Discord.GuildID,
		cfg.Site.BaseURL+"/auth/callback", nil)

	if !cfg.DiscordConfigured() {
		// Deliberately not fatal: bringing the service up and looking around
		// before creating the Discord application is a legitimate thing to
		// do, and the UI says so plainly.
		log.Warn("Discord is not configured — sign-in and alerts are disabled until discord.* is filled in")
	}

	region, err := geo.Load(cfg.Status.GeoJSONPath)
	if err != nil {
		// A status board configured to cover a region, that silently covers
		// the world instead, is worse than not starting.
		log.Error("loading the status region", "err", err)
		os.Exit(1)
	}
	if region != nil {
		log.Info("status region loaded", "name", region.Name, "polygons", region.Rings())
	}

	srv, err := web.New(st, dc, cfg, log)
	if err != nil {
		log.Error("building web server", "err", err)
		os.Exit(1)
	}

	srv.Region = region

	// The services the mesh depends on but that HopReact cannot infer from
	// packets. Probed on a timer and cached, so the public board never turns
	// into a request amplifier.
	if cfg.Status.Enabled && len(cfg.Status.Services) > 0 {
		checks := make([]health.Check, 0, len(cfg.Status.Services))
		for _, sc := range cfg.Status.Services {
			checks = append(checks, health.Check{
				Name: sc.Name, Kind: sc.Kind, Target: sc.Target, Note: sc.Note,
			})
		}
		srv.Health = &health.Monitor{
			Checks: checks, Interval: cfg.Status.ServiceInterval,
			Timeout: cfg.RequestTimeout(), Log: log,
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pl := &poller.Poller{
		Store: st, Scope: scope, Cfg: cfg, Log: log, Now: time.Now,
		OnBreaker: func(ctx context.Context, reason string) {
			if cfg.Discord.OperatorUserID == "" || !cfg.DiscordConfigured() {
				return
			}
			msg := "⚠️ **HopReact alert breaker tripped**\n" + reason +
				"\nNo user alerts were sent for this poll. Check the upstream feed."
			if err := dc.SendDM(ctx, cfg.Discord.OperatorUserID, msg); err != nil {
				log.Error("could not notify the operator about the breaker", "err", err)
			}
		},
	}
	nt := &notify.Notifier{Store: st, Sender: dc, Log: log, BaseURL: cfg.Site.BaseURL, Now: time.Now}

	pollTicker := time.NewTicker(cfg.Poll.Interval)
	defer pollTicker.Stop()
	go pl.Run(ctx, pollTicker.C)

	if cfg.DiscordConfigured() {
		drainTicker := time.NewTicker(drainInterval)
		defer drainTicker.Stop()
		go nt.Run(ctx, drainTicker.C)
		go runMembershipChecks(ctx, st, dc, log)

		// A bot is shown online only while it holds a Gateway connection —
		// REST calls alone leave it greyed out however healthy the process
		// is. Running one turns the member list into a free liveness
		// indicator: green means this process is up and talking to Discord.
		// Deliberately best-effort; presence never affects alerting.
		gw := &discord.Gateway{
			BotToken: cfg.Discord.BotToken,
			Log:      log,
			Status: func() string {
				n, err := st.CountTargets(ctx)
				if err != nil || n == 0 {
					return "the mesh"
				}
				w, err := st.CountWatches(ctx)
				if err != nil || w == 0 {
					return fmt.Sprintf("%d nodes", n)
				}
				return fmt.Sprintf("%d nodes · %d watched", n, w)
			},
		}
		go gw.Run(ctx)
	}
	if srv.Health != nil {
		go srv.Health.Run(ctx)
	}
	go runHousekeeping(ctx, st, log)

	httpSrv := &http.Server{
		Addr:              cfg.HTTP.Listen,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Info("listening", "addr", cfg.HTTP.Listen, "base_url", cfg.Site.BaseURL)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Error("shutdown", "err", err)
	}
}

// runMembershipChecks periodically confirms each user is still in the alert
// server, so a silent departure shows up on their dashboard rather than as
// alerts that simply never arrive.
func runMembershipChecks(ctx context.Context, st *store.Store, dc *discord.Client, log *slog.Logger) {
	t := time.NewTicker(membershipInterval)
	defer t.Stop()
	check := func() {
		users, err := st.UsersWithWatches(ctx)
		if err != nil {
			log.Error("listing users for membership check", "err", err)
			return
		}
		for _, u := range users {
			member, err := dc.CheckMembership(ctx, u.DiscordID)
			if err != nil {
				continue // transient; leave the current status alone
			}
			reason := ""
			if !member {
				reason = "You are no longer in the HopReact Discord server, so the bot cannot DM you."
			}
			if member != u.DMOK {
				if err := st.SetDMStatus(ctx, u.ID, member, reason); err != nil {
					log.Error("recording DM status", "err", err)
				}
			}
		}
	}
	check()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			check()
		}
	}
}

func runHousekeeping(ctx context.Context, st *store.Store, log *slog.Logger) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := st.DeleteExpiredSessions(ctx); err != nil {
				log.Error("pruning sessions", "err", err)
			} else if n > 0 {
				log.Info("pruned expired sessions", "count", n)
			}
			// poll_runs, delivered notifications and alert_events are the only
			// tables that grow with time rather than with the size of the
			// mesh — a poll every five minutes is ~105,000 rows a year on its
			// own. Everything else is one row per node, or per node and
			// payload type; no packet is ever stored.
			if n, err := st.Prune(ctx); err != nil {
				log.Error("pruning history", "err", err)
			} else if n > 0 {
				log.Info("pruned old history", "rows", n)
			}
		}
	}
}
