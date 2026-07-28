// Command hopreact watches a CoreScope instance and sends a Discord DM when
// a repeater or observer someone has claimed stops being seen.
//
// One process does both halves: an HTTP server for sign-in and the
// dashboard, and a single background poller that fetches CoreScope and
// evaluates every watch against it. They share one SQLite database, which
// is also why there must only ever be one instance running — see the
// single-instance lock (added with the poller).
//
// This file is currently the scaffold's entry point: it parses the config
// flag and reports the build, so the repository builds and CI is green from
// the first commit. The server and poller land in their own phases.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"

	"hopreact/internal/buildinfo"
)

func main() {
	// Same flag, same help string, as HopReach's binaries — the config path
	// is the only thing not itself configured in the config file.
	configFlag := flag.String("config", "", "path to config.yaml (default: $HOPREACT_CONFIG, then ./config.yaml)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(buildinfo.Version)
		return
	}

	slog.Info("hopreact starting", "version", buildinfo.Version, "config_flag", *configFlag)
	slog.Error("not implemented yet: this is the project scaffold")
	os.Exit(1)
}
