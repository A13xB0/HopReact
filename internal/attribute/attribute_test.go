package attribute

import (
	"testing"

	"hopreact/internal/corescope"
)

// Real public keys and real routes, lifted from a live ScotMesh packet
// sample rather than invented, so the widths and shapes here are the ones
// the code will actually meet.
const (
	keyAdvert  = "9bb42d96d691e57115b949bc394e9949e07a4451626c9052d5abd2cb5c43457e"
	key463f15  = "463f15467ceba721cf903a33e0da069627d86ddfa21f397a14a128fa166d8976"
	key630626  = "630626a3720eb9820833ad289e80e8d60ba0d7268465e2c08735cf21cc006866"
	keyE426c0  = "e426c0041016cdadfed8665741648402d577edae40b7a94e52066ce70ca4e7a4"
	key2eecdf  = "2eecdff2c19996b333c5059a40740dfa8bf8b803f306c23084269f8a39edf8b4"
	keyTwin    = "463f15aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	keyOneByte = "21ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
)

func nodes(keys ...string) []corescope.Observation {
	out := make([]corescope.Observation, 0, len(keys))
	for _, k := range keys {
		out = append(out, corescope.Observation{Kind: corescope.KindNode, Key: k})
	}
	return out
}

func hits(t *testing.T, p corescope.Packet, idx Index) map[string]Direction {
	t.Helper()
	got := map[string]Direction{}
	for _, h := range Attribute(p, idx) {
		if prev, dup := got[h.Key+"|"+string(h.Direction)]; dup {
			t.Errorf("duplicate hit for %s (%s)", h.Key, prev)
		}
		got[h.Key+"|"+string(h.Direction)] = h.Direction
	}
	return got
}

func TestAttribute(t *testing.T) {
	idx := BuildIndex(nodes(keyAdvert, key463f15, key630626, keyE426c0, key2eecdf, keyOneByte))

	tests := []struct {
		name   string
		packet corescope.Packet
		want   []string // "key|direction"
	}{
		{
			// An advert names its sender outright, so it needs no path at all.
			name: "advert attributes its originator as sent",
			packet: corescope.Packet{
				RouteType:   corescope.RouteFlood,
				PayloadType: corescope.TypeADVERT, AdvertPubKey: keyAdvert,
			},
			want: []string{keyAdvert + "|sent"},
		},
		{
			// A repeater re-broadcasting someone else's advert is carrying it.
			// This is what makes "adverts, including where it's in the path"
			// work as a signal for repeaters that never advertise much.
			name: "advert also attributes its relays as carried",
			packet: corescope.Packet{
				RouteType:    corescope.RouteFlood,
				PayloadType:  corescope.TypeADVERT,
				AdvertPubKey: keyAdvert,
				PathHops:     []string{"e426c0", "2eecdf"},
			},
			want: []string{keyAdvert + "|sent", keyE426c0 + "|carried", key2eecdf + "|carried"},
		},
		{
			// A real nine-hop RESPONSE. Encrypted, so nobody is attributed as
			// having sent it — only as having carried it.
			name: "encrypted traffic attributes hops only, never a sender",
			packet: corescope.Packet{
				RouteType:   corescope.RouteFlood,
				PayloadType: corescope.TypeRESPONSE,
				PathHops: []string{"463f15", "630626", "39c4ba", "6edc9b",
					"e426c0", "1360f7", "2eecdf", "04f1fe", "50480d"},
			},
			// Only the four hops that are nodes we know about resolve; the
			// rest are simply unknown, which is not an error.
			want: []string{key463f15 + "|carried", key630626 + "|carried",
				keyE426c0 + "|carried", key2eecdf + "|carried"},
		},
		{
			// The whole point of the three-byte rule. "21" belongs to exactly
			// one node in this index, so a naive implementation would resolve
			// it — but on the real mesh a one-byte hop matches up to eight
			// nodes, and a confident wrong attribution marks a dead node alive.
			name: "one-byte hops are discarded even when they look unique",
			packet: corescope.Packet{
				RouteType:   corescope.RouteFlood,
				PayloadType: corescope.TypeGRPTXT,
				PathHops:    []string{"21", "90", "44", "7a", "f2"},
			},
			want: nil,
		},
		{
			name: "two-byte hops are discarded too",
			packet: corescope.Packet{
				RouteType:   corescope.RouteFlood,
				PayloadType: corescope.TypeTXTMsg,
				PathHops:    []string{"463f", "6306"},
			},
			want: nil,
		},
		{
			// A route that lists the same node twice is one piece of evidence
			// about it, not two.
			name: "a repeated hop counts once",
			packet: corescope.Packet{
				RouteType:   corescope.RouteFlood,
				PayloadType: corescope.TypeTXTMsg,
				PathHops:    []string{"e426c0", "2eecdf", "e426c0"},
			},
			want: []string{keyE426c0 + "|carried", key2eecdf + "|carried"},
		},
		{
			// Nothing to say, and nothing to crash on.
			name: "a packet with no path and no advert key yields nothing",
			packet: corescope.Packet{RouteType: corescope.RouteFlood,
				PayloadType: corescope.TypeACK},
			want: nil,
		},
		{
			// An advert whose key is truncated or missing must not be
			// half-attributed to a prefix.
			name: "an advert without a full key attributes no sender",
			packet: corescope.Packet{
				RouteType:   corescope.RouteFlood,
				PayloadType: corescope.TypeADVERT, AdvertPubKey: "9bb42d96",
			},
			want: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := hits(t, tc.packet, idx)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d hits %v, want %d %v", len(got), got, len(tc.want), tc.want)
			}
			for _, w := range tc.want {
				if _, ok := got[w]; !ok {
					t.Errorf("missing hit %s (got %v)", w, got)
				}
			}
		})
	}
}

// A prefix shared by two nodes must resolve to neither. Attributing it to one
// of them would mark whichever node is actually dead as alive, and silently
// disarm the alert its owner is relying on.
func TestAmbiguousPrefixResolvesToNobody(t *testing.T) {
	idx := BuildIndex(nodes(key463f15, keyTwin, key630626))

	if _, ok := idx.Lookup("463f15"); ok {
		t.Error("a 3-byte prefix shared by two nodes must not resolve")
	}
	if k, ok := idx.Lookup("630626"); !ok || k != key630626 {
		t.Errorf("an unshared prefix should still resolve, got %q %v", k, ok)
	}

	p := corescope.Packet{RouteType: corescope.RouteFlood, PayloadType: corescope.TypeTXTMsg,
		PathHops: []string{"463f15", "630626"}}
	got := Attribute(p, idx)
	if len(got) != 1 || got[0].Key != key630626 {
		t.Errorf("only the unambiguous hop should be attributed, got %+v", got)
	}
}

// Observers report what they hear rather than forwarding it, so they never
// appear in a route. Indexing their ids would be inventing mesh identities.
func TestObserversAreNotIndexed(t *testing.T) {
	idx := BuildIndex([]corescope.Observation{
		{Kind: corescope.KindObserver, Key: key463f15},
		{Kind: corescope.KindNode, Key: key630626},
	})
	if idx.Len() != 1 {
		t.Fatalf("index has %d prefixes, want only the node's", idx.Len())
	}
	if _, ok := idx.Lookup("463f15"); ok {
		t.Error("an observer id must not resolve as a path hop")
	}
}

// The same node appearing twice in the feed (paged, or as both node and
// observer) must not make its own prefix look contested.
func TestDuplicateObservationsDoNotSelfCollide(t *testing.T) {
	idx := BuildIndex(nodes(key463f15, key463f15, key630626))
	if k, ok := idx.Lookup("463f15"); !ok || k != key463f15 {
		t.Errorf("a node listed twice should still resolve, got %q %v", k, ok)
	}
}

// A hop wider than three bytes still identifies its node by its first three.
func TestLongerHopsStillResolve(t *testing.T) {
	idx := BuildIndex(nodes(key463f15))
	if k, ok := idx.Lookup("463f15467c"); !ok || k != key463f15 {
		t.Errorf("a 5-byte hop should resolve on its first 3, got %q %v", k, ok)
	}
}

// A direct route's path is the route the SENDER chose, not a record of where
// the packet has been — each hop removes itself before forwarding
// (Mesh.cpp, removeSelfFromPath). A node still listed there is where the
// packet is going. Counting it as "carried" credits a repeater for work it
// has not done, and would keep a dead repeater looking alive purely because
// people are still trying to route through it.
//
// On the live mesh this was 1,886 of 21,882 attributions, most of them REQ
// packets on their way TO a repeater.
func TestDirectRoutePathIsIntentNotHistory(t *testing.T) {
	idx := BuildIndex(nodes(keyE426c0, key2eecdf))
	hops := []string{"e426c0", "2eecdf"}

	for _, rt := range []int{corescope.RouteDirect, corescope.RouteTransportDirect} {
		got := Attribute(corescope.Packet{
			RouteType: rt, PayloadType: corescope.TypeREQ, PathHops: hops}, idx)
		if len(got) != 0 {
			t.Errorf("route %d: attributed %d hops from a planned route, want 0", rt, len(got))
		}
	}
	// The same path on a flood route IS history, and must still count.
	for _, rt := range []int{corescope.RouteFlood, corescope.RouteTransportFlood} {
		got := Attribute(corescope.Packet{
			RouteType: rt, PayloadType: corescope.TypeREQ, PathHops: hops}, idx)
		if len(got) != 2 {
			t.Errorf("route %d: attributed %d hops from a built-up path, want 2", rt, len(got))
		}
	}
}

// An advert names its sender in the clear, so that half stands regardless of
// how the packet was routed.
func TestAdvertSenderSurvivesADirectRoute(t *testing.T) {
	idx := BuildIndex(nodes(keyAdvert, keyE426c0))
	got := Attribute(corescope.Packet{
		RouteType: corescope.RouteDirect, PayloadType: corescope.TypeADVERT,
		AdvertPubKey: keyAdvert, PathHops: []string{"e426c0"}}, idx)
	if len(got) != 1 || got[0].Direction != DirSent || got[0].Key != keyAdvert {
		t.Fatalf("got %+v, want just the sender", got)
	}
}

// TRACE stores SNR readings in its path field rather than hashes — "append
// SNR (Not hash!)" in Mesh.cpp. Those bytes are not identities, so any match
// against a node prefix is coincidence and must never be recorded.
func TestTracePathIsNeverAttributed(t *testing.T) {
	idx := BuildIndex(nodes(keyE426c0, key2eecdf))
	got := Attribute(corescope.Packet{
		RouteType:   corescope.RouteFlood,
		PayloadType: corescope.TypeTRACE,
		// These would resolve if they were hashes. They are signal readings.
		PathHops: []string{"e426c0", "2eecdf"}}, idx)
	if len(got) != 0 {
		t.Errorf("attributed %d hops from a TRACE's SNR path, want 0", len(got))
	}
}
