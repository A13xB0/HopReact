// Package health checks the services a mesh depends on but that HopReact does
// not otherwise touch: the MQTT broker observers report through, and the web
// services alongside it.
//
// Deliberately shallow. A TCP connect or an HTTP status is enough to say "the
// thing is answering", and anything deeper would mean HopReact knowing how
// each service works — which is how a status page ends up lying because its
// own probe broke.
package health

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"sync"
	"time"
)

// Check is one thing to watch.
type Check struct {
	Name string
	// Kind is "http" or "tcp".
	Kind string
	// Target is a URL for http, or host:port for tcp.
	Target string
	// Note explains what this service does, for the hover.
	Note string
}

// Result is the outcome of the last probe.
type Result struct {
	Check
	OK      bool
	Detail  string
	Latency time.Duration
	At      time.Time
}

// Monitor probes a fixed set of checks on a timer and keeps the last result.
//
// Results are served from memory: a public page must never turn into a
// request amplifier, dialling four services every time somebody refreshes.
type Monitor struct {
	Checks   []Check
	Interval time.Duration
	Timeout  time.Duration
	Log      *slog.Logger

	mu      sync.RWMutex
	results map[string]Result
}

// Results returns the latest probe of each check, in configured order.
func (m *Monitor) Results() []Result {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Result, 0, len(m.Checks))
	for _, c := range m.Checks {
		if r, ok := m.results[c.Name]; ok {
			out = append(out, r)
		} else {
			out = append(out, Result{Check: c, Detail: "not checked yet"})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return false }) // keep configured order
	return out
}

// Probe runs every check once. Used at startup and by tests that want a
// result without waiting for a tick.
func (m *Monitor) Probe(ctx context.Context) {
	if m.Timeout <= 0 {
		m.Timeout = 10 * time.Second
	}
	m.probeAll(ctx)
}

// Run probes immediately, then on every tick, until ctx is cancelled.
func (m *Monitor) Run(ctx context.Context) {
	if len(m.Checks) == 0 {
		return
	}
	if m.Interval <= 0 {
		m.Interval = time.Minute
	}
	if m.Timeout <= 0 {
		m.Timeout = 10 * time.Second
	}
	m.probeAll(ctx)
	t := time.NewTicker(m.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.probeAll(ctx)
		}
	}
}

func (m *Monitor) probeAll(ctx context.Context) {
	var wg sync.WaitGroup
	for _, c := range m.Checks {
		wg.Add(1)
		go func(c Check) {
			defer wg.Done()
			r := m.probe(ctx, c)
			m.mu.Lock()
			if m.results == nil {
				m.results = map[string]Result{}
			}
			m.results[c.Name] = r
			m.mu.Unlock()
		}(c)
	}
	wg.Wait()
}

func (m *Monitor) probe(ctx context.Context, c Check) Result {
	ctx, cancel := context.WithTimeout(ctx, m.Timeout)
	defer cancel()
	start := time.Now()
	r := Result{Check: c, At: start.UTC()}

	switch c.Kind {
	case "tcp":
		// Enough for a broker: if it accepts a connection it is running. We
		// do not speak MQTT at it — a probe that has to log in is a probe
		// that can break on its own.
		var d net.Dialer
		conn, err := d.DialContext(ctx, "tcp", c.Target)
		if err != nil {
			r.Detail = err.Error()
			return r
		}
		conn.Close()
		r.OK, r.Latency, r.Detail = true, time.Since(start), "accepting connections"

	default:
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Target, nil)
		if err != nil {
			r.Detail = err.Error()
			return r
		}
		req.Header.Set("User-Agent", "HopReact status probe")
		client := &http.Client{
			Timeout: m.Timeout,
			Transport: &http.Transport{
				TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
				DisableKeepAlives:   true,
				TLSHandshakeTimeout: m.Timeout,
			},
		}
		resp, err := client.Do(req)
		if err != nil {
			r.Detail = err.Error()
			return r
		}
		defer resp.Body.Close()
		r.Latency = time.Since(start)
		// Any answer that is not a server error counts: a redirect or an
		// auth challenge still proves the service is up and answering.
		r.OK = resp.StatusCode < 500
		r.Detail = resp.Status
	}
	return r
}
