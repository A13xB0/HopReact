package corescope

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// Field-for-field copies of real responses from the live ScotMesh instance,
// trimmed to the fields this package reads. Kept verbatim (including the
// emoji in an observer name and the null lat/lon) so the parser is tested
// against the shapes the API genuinely returns, not idealised ones.
const realNodeJSON = `{"nodes":[{
  "advert_count":132,"battery_mv":null,"first_seen":"2026-07-14T14:51:24Z",
  "hash_size":1,"last_heard":"2026-07-28T17:58:40Z","last_relayed":"2026-07-28T17:49:58Z",
  "last_seen":"2026-07-28T17:58:40Z","lat":55.93352,"lon":-3.21313,"name":"NUMC-MC",
  "public_key":"D41EE22644B0EA3AEE70958CC5F4E87A1CFDBB1F404396DC0A7BE3E7030DF741",
  "relay_active":true,"relay_count_1h":25,"relay_count_24h":507,"role":"repeater"
}],"total":1,"counts":{"repeaters":1}}`

const realObserverJSON = `{"observers":[{
  "id":"04F1FE66A27B35A586704323DE2267C8B11CD0719E5AF499035A0BC69E3689E8",
  "name":"Cadham Village 🏘️","iata":"EDI","last_seen":"2026-07-28T18:00:32Z",
  "first_seen":"2026-07-14T12:13:10Z","packet_count":60066,
  "last_packet_at":"2026-07-28T18:00:32Z","packetsLastHour":129,
  "lat":56.205568,"lon":-3.161287,"nodeRole":"repeater"
},{
  "id":"8911F060F0BC29B3740128603F34C289E2EE72B0D9CFFE997FA7B2111D96F09C",
  "name":"NOC-PYMC-JLO","iata":"NOC","last_seen":"2026-07-28T18:00:32Z",
  "first_seen":null,"packet_count":161019,"last_packet_at":"2026-07-28T18:00:32Z",
  "packetsLastHour":219,"lat":null,"lon":null,"nodeRole":null
}]}`

func serve(t *testing.T, nodes, observers string) *Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/nodes", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, nodes)
	})
	mux.HandleFunc("/api/observers", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, observers)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, 5*time.Second, srv.Client())
}

func TestFetchParsesRealResponses(t *testing.T) {
	c := serve(t, realNodeJSON, realObserverJSON)
	snap, err := c.Fetch(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	if len(snap.Nodes) != 1 {
		t.Fatalf("got %d nodes, want 1", len(snap.Nodes))
	}
	n := snap.Nodes[0]
	// The live API returns public_key uppercase and observer ids uppercase
	// too; everything downstream joins on this string, so it must be
	// normalised here or a watch will silently never match its target.
	if want := "d41ee22644b0ea3aee70958cc5f4e87a1cfdbb1f404396dc0a7be3e7030df741"; n.Key != want {
		t.Errorf("Key = %q, want it lowercased to %q", n.Key, want)
	}
	if n.Kind != KindNode {
		t.Errorf("Kind = %q, want %q", n.Kind, KindNode)
	}
	if n.Name != "NUMC-MC" || n.Role != "repeater" {
		t.Errorf("Name/Role = %q/%q", n.Name, n.Role)
	}
	if want := time.Date(2026, 7, 28, 17, 58, 40, 0, time.UTC); !n.LastSeen.Equal(want) {
		t.Errorf("LastSeen = %v, want %v", n.LastSeen, want)
	}
	if want := time.Date(2026, 7, 28, 17, 49, 58, 0, time.UTC); !n.LastRelayed.Equal(want) {
		t.Errorf("LastRelayed = %v, want %v", n.LastRelayed, want)
	}
	if n.RelayCount1h != 25 || n.RelayCount24h != 507 {
		t.Errorf("relay counts = %d/%d, want 25/507", n.RelayCount1h, n.RelayCount24h)
	}

	if len(snap.Observers) != 2 {
		t.Fatalf("got %d observers, want 2", len(snap.Observers))
	}
	o := snap.Observers[0]
	if o.Kind != KindObserver {
		t.Errorf("Kind = %q, want %q", o.Kind, KindObserver)
	}
	if want := "04f1fe66a27b35a586704323de2267c8b11cd0719e5af499035a0bc69e3689e8"; o.Key != want {
		t.Errorf("observer Key = %q, want it lowercased", o.Key)
	}
	if want := time.Date(2026, 7, 28, 18, 0, 32, 0, time.UTC); !o.LastSeen.Equal(want) {
		t.Errorf("observer LastSeen = %v, want last_packet_at %v", o.LastSeen, want)
	}
	// An observer reports what it hears; it never forwards. Leaving this
	// zero is what stops the relay alert ever firing for one.
	if !o.LastRelayed.IsZero() {
		t.Errorf("observer LastRelayed = %v, want zero", o.LastRelayed)
	}
	// Null lat/lon on the second observer must survive as nil, not 0/0 —
	// which is a real location in the Atlantic.
	if snap.Observers[1].Lat != nil || snap.Observers[1].Lon != nil {
		t.Error("null lat/lon should stay nil rather than becoming 0")
	}

	if got := len(snap.All()); got != 3 {
		t.Errorf("All() returned %d, want 3", got)
	}
}

// 86 repeaters on the live instance have never relayed. Their null
// last_relayed has to stay distinguishable from "relayed long ago", or the
// relay alert fires for nodes that never had the chance to stop.
func TestNullLastRelayedStaysZero(t *testing.T) {
	const js = `{"nodes":[{"public_key":"aa","name":"Never Relayed","role":"repeater",
	  "last_seen":"2026-07-28T17:58:40Z","last_relayed":null}],"total":1}`
	c := serve(t, js, `{"observers":[]}`)

	nodes, err := c.FetchNodes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 {
		t.Fatalf("got %d nodes", len(nodes))
	}
	if !nodes[0].LastRelayed.IsZero() {
		t.Errorf("LastRelayed = %v, want the zero time for a null", nodes[0].LastRelayed)
	}
	if nodes[0].LastSeen.IsZero() {
		t.Error("LastSeen should still be set")
	}
}

// last_seen leads, last_heard is the fallback.
func TestLastSeenFallsBackToLastHeard(t *testing.T) {
	const js = `{"nodes":[{"public_key":"bb","last_seen":null,
	  "last_heard":"2026-07-28T10:00:00Z"}],"total":1}`
	c := serve(t, js, `{"observers":[]}`)
	nodes, _ := c.FetchNodes(context.Background())
	want := time.Date(2026, 7, 28, 10, 0, 0, 0, time.UTC)
	if len(nodes) != 1 || !nodes[0].LastSeen.Equal(want) {
		t.Errorf("LastSeen = %v, want the last_heard fallback %v", nodes[0].LastSeen, want)
	}
}

// One unparseable timestamp must cost that node its signal, not fail the
// whole poll — a failed poll suppresses evaluation for every watch, so
// being strict here would let a single bad record blind the service.
func TestGarbageTimestampDoesNotFailThePoll(t *testing.T) {
	const js = `{"nodes":[
	  {"public_key":"aa","last_seen":"not a timestamp"},
	  {"public_key":"bb","last_seen":"2026-07-28T10:00:00Z"}],"total":2}`
	c := serve(t, js, `{"observers":[]}`)

	nodes, err := c.FetchNodes(context.Background())
	if err != nil {
		t.Fatalf("a malformed timestamp must not fail the fetch: %v", err)
	}
	if len(nodes) != 2 {
		t.Fatalf("got %d nodes, want both kept", len(nodes))
	}
	if !nodes[0].LastSeen.IsZero() {
		t.Error("the unparseable timestamp should degrade to zero")
	}
	if nodes[1].LastSeen.IsZero() {
		t.Error("the valid neighbour should be unaffected")
	}
}

// The live instance returns ~780 nodes against a 500 page size, so
// pagination is the normal path, not an edge case.
func TestFetchNodesPaginates(t *testing.T) {
	const total = 1250
	var requests []string

	mux := http.NewServeMux()
	mux.HandleFunc("/api/nodes", func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.RawQuery)
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		var page []map[string]any
		for i := offset; i < offset+limit && i < total; i++ {
			page = append(page, map[string]any{
				"public_key": fmt.Sprintf("%064x", i),
				"role":       "repeater",
				"last_seen":  "2026-07-28T10:00:00Z",
			})
		}
		json.NewEncoder(w).Encode(map[string]any{"nodes": page, "total": total})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewClient(srv.URL, 5*time.Second, srv.Client())
	nodes, err := c.FetchNodes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != total {
		t.Fatalf("got %d nodes, want %d", len(nodes), total)
	}
	if len(requests) != 3 { // 500 + 500 + 250
		t.Errorf("made %d requests %v, want 3 pages", len(requests), requests)
	}
	seen := map[string]bool{}
	for _, n := range nodes {
		if seen[n.Key] {
			t.Fatalf("duplicate key %s — pagination overlapped", n.Key)
		}
		seen[n.Key] = true
	}
}

// A server that always returns a full page must not spin forever.
func TestFetchNodesStopsOnAPathologicalServer(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/nodes", func(w http.ResponseWriter, r *http.Request) {
		page := make([]map[string]any, pageLimit)
		for i := range page {
			page[i] = map[string]any{"public_key": fmt.Sprintf("%064x", i)}
		}
		// total omitted, so the offset >= total exit can never trigger.
		json.NewEncoder(w).Encode(map[string]any{"nodes": page})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewClient(srv.URL, 5*time.Second, srv.Client())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := c.FetchNodes(context.Background()); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("FetchNodes did not terminate against a server that always returns a full page")
	}
}

// Either endpoint failing has to fail the whole poll. A half-poll would
// reach the evaluator as "every observer went offline at once", which is
// exactly the mass false alarm the design exists to prevent.
func TestFetchFailsWhenEitherEndpointFails(t *testing.T) {
	t.Run("nodes fail", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/api/nodes", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		})
		mux.HandleFunc("/api/observers", func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"observers":[]}`)
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()
		c := NewClient(srv.URL, 5*time.Second, srv.Client())
		if _, err := c.Fetch(context.Background(), time.Now()); err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("observers fail", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/api/nodes", func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"nodes":[],"total":0}`)
		})
		mux.HandleFunc("/api/observers", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusBadGateway)
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()
		c := NewClient(srv.URL, 5*time.Second, srv.Client())
		if _, err := c.Fetch(context.Background(), time.Now()); err == nil {
			t.Fatal("expected an error")
		}
	})
}

func TestFetchRespectsContextCancellation(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/nodes", func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewClient(srv.URL, 5*time.Second, srv.Client())
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := c.FetchNodes(ctx); err == nil {
		t.Fatal("expected the cancelled context to surface as an error")
	}
}

// A trailing slash on the configured URL must not produce "//api/nodes".
func TestNewClientTrimsTrailingSlash(t *testing.T) {
	c := NewClient("https://example.com/", time.Second, nil)
	if c.BaseURL != "https://example.com" {
		t.Errorf("BaseURL = %q, want the trailing slash trimmed", c.BaseURL)
	}
	if c.HTTP.Timeout != time.Second {
		t.Errorf("a nil http client must get the timeout, got %v", c.HTTP.Timeout)
	}
}
