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
	"io"
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

// defaultPacketLimit is how many packets one poll reads. The live mesh runs
// about 5 packets a minute, so this covers roughly two hours — ample overlap
// for a five-minute poll, and enough that a few missed polls lose nothing.
const defaultPacketLimit = 600

// maxPacketLimit matches CoreScope's own ceiling on ?limit.
const maxPacketLimit = 10000

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

// ---------------------------------------------------------------- packets --

// Payload types, from MeshCore's firmware (Packet.h) via CoreScope's decoder.
// CoreScope emits payload_type as a bare integer on packet objects and never
// as a name, so the mapping has to live here.
const (
	TypeREQ       = 0
	TypeRESPONSE  = 1
	TypeTXTMsg    = 2
	TypeACK       = 3
	TypeADVERT    = 4
	TypeGRPTXT    = 5
	TypeGRPData   = 6
	TypeANONReq   = 7
	TypePATH      = 8
	TypeTRACE     = 9
	TypeMULTIPART = 10
	TypeCONTROL   = 11
	TypeRAWCustom = 15
)

// TypeName renders a payload type for display. Unknown values keep their
// number rather than being hidden — 12, 13 and 14 are unused today but a
// firmware release could claim them.
func TypeName(t int) string {
	switch t {
	case TypeREQ:
		return "REQ"
	case TypeRESPONSE:
		return "RESPONSE"
	case TypeTXTMsg:
		return "TXT_MSG"
	case TypeACK:
		return "ACK"
	case TypeADVERT:
		return "ADVERT"
	case TypeGRPTXT:
		return "GRP_TXT"
	case TypeGRPData:
		return "GRP_DATA"
	case TypeANONReq:
		return "ANON_REQ"
	case TypePATH:
		return "PATH"
	case TypeTRACE:
		return "TRACE"
	case TypeMULTIPART:
		return "MULTIPART"
	case TypeCONTROL:
		return "CONTROL"
	case TypeRAWCustom:
		return "RAW_CUSTOM"
	}
	return fmt.Sprintf("TYPE_%d", t)
}

// AllTypes is every payload type HopReact knows, in wire order.
var AllTypes = []int{
	TypeREQ, TypeRESPONSE, TypeTXTMsg, TypeACK, TypeADVERT, TypeGRPTXT,
	TypeGRPData, TypeANONReq, TypePATH, TypeTRACE, TypeMULTIPART,
	TypeCONTROL, TypeRAWCustom,
}

// Packet is one transmission, reduced to what attribution needs.
type Packet struct {
	ID          int64
	At          time.Time
	PayloadType int
	// PathHops are the route's hop hashes, lowercased, exactly as they
	// appeared on the wire. Width varies between packets (it is declared once
	// per packet by whoever originated it) and the attributor discards
	// anything under three bytes.
	PathHops []string
	// AdvertPubKey is the originator's full public key, lowercased. Only
	// ADVERTs carry one: every other payload type is encrypted and identifies
	// its sender with a single byte, which on this mesh is ~3 candidates.
	AdvertPubKey string
}

type packetJSON struct {
	ID          int64    `json:"id"`
	FirstSeen   *string  `json:"first_seen"`
	Timestamp   *string  `json:"timestamp"`
	PayloadType *int     `json:"payload_type"`
	ParsedPath  []string `json:"_parsedPath"`
	DecodedJSON string   `json:"decoded_json"`
}

// decodedAdvert is the sliver of decoded_json that matters. Note pubKey sits
// at the top level of that document, not under a "payload" object.
type decodedAdvert struct {
	PubKey string `json:"pubKey"`
}

// FetchPackets returns the most recent packets, newest first.
//
// Deliberately unfiltered. CoreScope does accept ?node= and ?type=, but both
// defeat its in-memory fast path and force a full store scan and sort on
// every request, and ?node= means different things depending on whether the
// request is served from memory (originated-or-relayed-or-addressed-to) or
// falls back to SQL (ADVERT originator only). One unfiltered page avoids that
// ambiguity entirely, and costs the same regardless of how many nodes are
// being watched.
func (c *Client) FetchPackets(ctx context.Context, limit int) ([]Packet, error) {
	return c.fetchPackets(ctx, time.Time{}, limit)
}

// FetchPacketsSince returns every packet CoreScope has recorded since a given
// time, newest first.
//
// Used for the one-off backfill that gives a fresh install real history to
// show, instead of a table reading "never" against every payload type until a
// day's traffic has trickled through. CoreScope does the windowing, so this
// costs one request: 24 hours of the live mesh is about 4,600 packets and
// under five megabytes.
func (c *Client) FetchPacketsSince(ctx context.Context, since time.Time, limit int) ([]Packet, error) {
	return c.fetchPackets(ctx, since, limit)
}

func (c *Client) fetchPackets(ctx context.Context, since time.Time, limit int) ([]Packet, error) {
	if limit <= 0 {
		limit = defaultPacketLimit
	}
	if limit > maxPacketLimit {
		limit = maxPacketLimit
	}
	u := fmt.Sprintf("%s/api/packets?limit=%d", c.BaseURL, limit)
	if !since.IsZero() {
		u += "&since=" + url.QueryEscape(since.UTC().Format(time.RFC3339))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("corescope: building request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("corescope: GET %s: %w", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("corescope: GET %s: status %d", u, resp.StatusCode)
	}

	// Decoded element by element rather than into one big struct. A backfill
	// page is several megabytes of JSON, most of it decoded_json and raw_hex
	// that we throw away immediately; materialising all of it at once would
	// cost several times the reduced form, and this runs in a 256MB container.
	return decodePacketStream(resp.Body)
}

func decodePacketStream(r io.Reader) ([]Packet, error) {
	dec := json.NewDecoder(r)

	// Walk to the value of the "packets" key.
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return nil, nil // no packets key at all; nothing to do
		}
		if err != nil {
			return nil, fmt.Errorf("corescope: decoding packets: %w", err)
		}
		if k, ok := tok.(string); ok && k == "packets" {
			break
		}
	}
	// Opening bracket of the array, or null.
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("corescope: decoding packets: %w", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return nil, nil
	}

	var out []Packet
	for dec.More() {
		var p packetJSON
		if err := dec.Decode(&p); err != nil {
			return nil, fmt.Errorf("corescope: decoding a packet: %w", err)
		}
		if p.PayloadType == nil {
			continue // nothing to attribute a type to
		}
		pk := Packet{ID: p.ID, PayloadType: *p.PayloadType, At: firstTime(p.FirstSeen, p.Timestamp)}
		for _, h := range p.ParsedPath {
			// CoreScope emits these uppercase; every comparison downstream is
			// against lowercased public keys.
			if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
				pk.PathHops = append(pk.PathHops, h)
			}
		}
		if pk.PayloadType == TypeADVERT && p.DecodedJSON != "" {
			var d decodedAdvert
			if json.Unmarshal([]byte(p.DecodedJSON), &d) == nil {
				pk.AdvertPubKey = strings.ToLower(strings.TrimSpace(d.PubKey))
			}
		}
		out = append(out, pk)
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
