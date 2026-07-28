// Package corescope reads the two CoreScope endpoints HopReact needs and
// normalises both into a single Observation type, so nothing downstream has
// to care whether a thing being watched is a mesh node or an observer
// station.
//
// The important discovery behind this package: CoreScope already computes
// "when did this node last appear in a packet route". /api/nodes carries
// last_relayed, derived from CoreScope's own path-hop index. So HopReact
// does NOT need to page backwards through the raw packet feed resolving hop
// prefixes to public keys — the expensive, lossy approach the problem
// initially seems to demand. Polling is two cheap unauthenticated GETs.
//
// (HopReach's own CoreScope client declares only a subset of these node
// fields and knows nothing about /api/observers. It is not a guide to the
// API's real surface.)
package corescope

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Kind distinguishes the two things that can be watched. They are separate
// even when they share a public key — and 7 of 9 observers on the live
// instance do — because one physical box can stop relaying while still
// reporting, or stop reporting while still relaying. Those are different
// failures and a user may care about either.
type Kind string

const (
	KindNode     Kind = "node"
	KindObserver Kind = "observer"
)

// pageLimit is the page size for /api/nodes. The live instance returns ~780
// nodes, so this is normally two requests.
const pageLimit = 500

// maxPages bounds the pagination loop so a server that keeps returning a
// full page forever can't spin here indefinitely.
const maxPages = 100

// Observation is one watchable thing at one moment, normalised from either
// endpoint.
//
// Times are zero (check with IsZero) when CoreScope has no value, which is a
// meaningfully different state from "long ago" — a node that has never
// relayed must never be reported as having *stopped* relaying, and 86
// repeaters on the live instance are in exactly that position.
type Observation struct {
	Kind Kind
	// Key is the 64-hex public key, lowercased. CoreScope spells it
	// public_key on nodes and id on observers, and cases them
	// inconsistently between the two, so it is normalised here — this is
	// the join key for everything downstream.
	Key  string
	Name string
	// Role is CoreScope's node role ("repeater", "companion", "room").
	// Empty for observers, which have no equivalent.
	Role string

	// LastSeen is the freshness signal alerts use by default: for a node,
	// when it was last heard at all; for an observer, when it last reported
	// a packet.
	LastSeen time.Time
	// LastRelayed is when this node last appeared as a hop in a packet
	// route. Zero means never. Always zero for observers.
	LastRelayed time.Time

	RelayCount1h  int
	RelayCount24h int
	Lat, Lon      *float64
}

// Snapshot is everything one poll retrieved.
type Snapshot struct {
	Nodes     []Observation
	Observers []Observation
	FetchedAt time.Time
}

// All returns nodes and observers together, which is what the alert
// evaluator wants — it looks watches up by (Kind, Key) and doesn't care
// which endpoint a thing came from.
func (s Snapshot) All() []Observation {
	out := make([]Observation, 0, len(s.Nodes)+len(s.Observers))
	out = append(out, s.Nodes...)
	out = append(out, s.Observers...)
	return out
}

// Client reads one CoreScope instance. Safe for concurrent use.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// NewClient returns a Client for baseURL. A nil httpClient gets one with the
// given timeout — never http.DefaultClient, which has no timeout at all and
// would let a hung CoreScope connection wedge the poller indefinitely.
func NewClient(baseURL string, timeout time.Duration, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: timeout}
	}
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), HTTP: httpClient}
}

// Fetch retrieves both endpoints. Either failing fails the whole poll: a
// half-poll would look like "every observer went offline at once" to the
// evaluator, which is precisely the false alarm the design exists to avoid.
func (c *Client) Fetch(ctx context.Context, now time.Time) (Snapshot, error) {
	nodes, err := c.FetchNodes(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	observers, err := c.FetchObservers(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{Nodes: nodes, Observers: observers, FetchedAt: now}, nil
}

// nodeJSON is the subset of GET /api/nodes this service reads. CoreScope
// returns considerably more per node; everything not needed for liveness is
// deliberately left out rather than carried around.
type nodeJSON struct {
	PublicKey     string   `json:"public_key"`
	Name          *string  `json:"name"`
	Role          string   `json:"role"`
	LastSeen      *string  `json:"last_seen"`
	LastHeard     *string  `json:"last_heard"`
	LastRelayed   *string  `json:"last_relayed"`
	RelayCount1h  *int     `json:"relay_count_1h"`
	RelayCount24h *int     `json:"relay_count_24h"`
	Lat           *float64 `json:"lat"`
	Lon           *float64 `json:"lon"`
}

type nodesResponse struct {
	Nodes []nodeJSON `json:"nodes"`
	Total int        `json:"total"`
}

// FetchNodes returns every node, following CoreScope's limit/offset
// pagination.
func (c *Client) FetchNodes(ctx context.Context) ([]Observation, error) {
	var out []Observation
	offset := 0
	for page := 0; page < maxPages; page++ {
		u := fmt.Sprintf("%s/api/nodes?limit=%d&offset=%d", c.BaseURL, pageLimit, offset)
		var resp nodesResponse
		if err := c.getJSON(ctx, u, &resp); err != nil {
			return nil, err
		}
		if len(resp.Nodes) == 0 {
			break
		}
		for _, n := range resp.Nodes {
			if n.PublicKey == "" {
				continue // no identity, nothing to watch
			}
			obs := Observation{
				Kind:          KindNode,
				Key:           strings.ToLower(n.PublicKey),
				Name:          deref(n.Name),
				Role:          n.Role,
				LastRelayed:   parseTime(n.LastRelayed),
				RelayCount1h:  derefInt(n.RelayCount1h),
				RelayCount24h: derefInt(n.RelayCount24h),
				Lat:           n.Lat,
				Lon:           n.Lon,
			}
			// last_seen and last_heard are the same value on the live
			// instance, but only last_seen is guaranteed populated, so it
			// leads and last_heard is the fallback.
			obs.LastSeen = firstTime(n.LastSeen, n.LastHeard)
			out = append(out, obs)
		}
		offset += len(resp.Nodes)
		if resp.Total > 0 && offset >= resp.Total {
			break
		}
	}
	return out, nil
}

// observerJSON is the subset of GET /api/observers this service reads.
type observerJSON struct {
	ID           string   `json:"id"`
	Name         *string  `json:"name"`
	LastSeen     *string  `json:"last_seen"`
	LastPacketAt *string  `json:"last_packet_at"`
	Lat          *float64 `json:"lat"`
	Lon          *float64 `json:"lon"`
}

type observersResponse struct {
	Observers []observerJSON `json:"observers"`
}

// FetchObservers returns every observer station.
//
// An observer's liveness question is "is it still reporting packets", so
// last_packet_at is the signal, with last_seen as a fallback for an
// instance that predates it. There is no relay concept here — an observer
// reports what it hears, it doesn't forward — so LastRelayed stays zero and
// the relay alert can never fire for one.
func (c *Client) FetchObservers(ctx context.Context) ([]Observation, error) {
	u := c.BaseURL + "/api/observers"
	var resp observersResponse
	if err := c.getJSON(ctx, u, &resp); err != nil {
		return nil, err
	}
	out := make([]Observation, 0, len(resp.Observers))
	for _, o := range resp.Observers {
		if o.ID == "" {
			continue
		}
		out = append(out, Observation{
			Kind:     KindObserver,
			Key:      strings.ToLower(o.ID),
			Name:     deref(o.Name),
			LastSeen: firstTime(o.LastPacketAt, o.LastSeen),
			Lat:      o.Lat,
			Lon:      o.Lon,
		})
	}
	return out, nil
}

func (c *Client) getJSON(ctx context.Context, rawURL string, dst any) error {
	if _, err := url.Parse(rawURL); err != nil {
		return fmt.Errorf("corescope: bad url %q: %w", rawURL, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("corescope: building request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("corescope: GET %s: %w", rawURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("corescope: GET %s: status %d", rawURL, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		return fmt.Errorf("corescope: decoding %s: %w", rawURL, err)
	}
	return nil
}

// parseTime converts one of CoreScope's RFC3339 timestamps, returning the
// zero time for nil, empty or unparseable input.
//
// Unparseable deliberately degrades to zero rather than erroring: a single
// malformed timestamp should make that one node's signal unknown, not fail
// the entire poll and leave every watch unevaluated.
func parseTime(s *string) time.Time {
	if s == nil || strings.TrimSpace(*s) == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, *s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// firstTime returns the first of its arguments that parses to a non-zero
// time.
func firstTime(candidates ...*string) time.Time {
	for _, c := range candidates {
		if t := parseTime(c); !t.IsZero() {
			return t
		}
	}
	return time.Time{}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefInt(i *int) int {
	if i == nil {
		return 0
	}
	return *i
}
