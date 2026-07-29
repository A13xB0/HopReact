// Package attribute works out which nodes a packet proves were involved, and
// in what capacity. It is pure: no I/O, no clock, no database, so the whole
// policy is table-testable.
//
// Two facts about MeshCore drive everything here, both measured against the
// live ScotMesh instance rather than assumed.
//
// # Only adverts identify their sender
//
// An ADVERT carries the originator's full 64-hex public key in the clear. In a
// 3,000-packet sample that was true of 431 out of 431 adverts. Every other
// payload type is encrypted and identifies its sender with a single byte,
// which across 779 nodes narrows it to about three candidates and sometimes
// eight. So "this node sent a message" is not a question this data can answer,
// for anyone, ever. "This node carried a message" is — and that is the
// distinction Direction exists to keep honest.
//
// # A path is only history on a flood route
//
// On a flood route each relay appends its own hash, so the path records who
// really carried the packet. On a direct route the path is the route the
// SENDER chose, and each hop removes itself before forwarding — a node still
// listed there has not touched the packet yet. Reading a direct path as
// history credits repeaters for work they have not done.
//
// TRACE is a special case on top of that: its path field carries SNR
// readings rather than hashes, so those bytes are not identities at all.
//
// # A hop is only an identification at three bytes
//
// Path hops are prefixes of the relaying node's public key, and their width is
// declared once per packet by whoever originated it — every packet in the
// sample had a uniform hop width. Across all 779 nodes:
//
//	1 byte  → 533 nodes share a prefix with someone (worst case 8 candidates)
//	2 bytes → 5 nodes share
//	3 bytes → no collisions at all
//
// So three bytes is where a hop stops being a guess. Shorter hops are
// discarded rather than resolved, which is why roughly 41% of packets
// contribute evidence and the rest contribute none.
//
// The cost of that choice is the thing to keep in mind when changing this
// package: absence of evidence here is NOT evidence of absence. A node can be
// busy on paths that happen to be one byte wide and produce nothing at all.
// Callers must treat "no evidence" as unknown, never as silence — see the
// no-evidence guard in the poller.
package attribute

import (
	"hopreact/internal/corescope"
)

// prefixBytes is the hop width below which a hop identifies nobody.
const prefixBytes = 3

// prefixHex is that width in hex characters.
const prefixHex = prefixBytes * 2

// Direction is how a node was involved in a packet.
type Direction string

const (
	// DirSent means the node originated the packet. Only ever derivable for
	// adverts.
	DirSent Direction = "sent"
	// DirCarried means the node appeared as a hop in the packet's route.
	DirCarried Direction = "carried"
)

// Hit is one thing a packet proves.
type Hit struct {
	Key       string
	Type      int
	Direction Direction
}

// Index resolves unambiguous 3-byte prefixes to public keys.
type Index struct {
	byPrefix map[string]string
}

// BuildIndex maps every 3-byte prefix that belongs to exactly one node.
//
// Prefixes shared by two or more nodes are omitted rather than resolved to
// one of them. There are none on the mesh today, but it grows, and a
// confident wrong attribution is worse than a missing one: it would mark a
// dead node as alive and suppress the alert someone is relying on.
//
// Observers are skipped. Their id is a station identifier, not a mesh public
// key, and they never appear in packet routes — an observer reports what it
// hears rather than forwarding it.
func BuildIndex(obs []corescope.Observation) Index {
	// Count first, then keep only the unique ones. Resolving as we go would
	// let the first node to claim a prefix keep it.
	count := make(map[string]int, len(obs))
	owner := make(map[string]string, len(obs))
	for _, o := range obs {
		if o.Kind != corescope.KindNode || len(o.Key) < prefixHex {
			continue
		}
		p := o.Key[:prefixHex]
		if owner[p] != o.Key {
			count[p]++
			owner[p] = o.Key
		}
	}
	idx := Index{byPrefix: make(map[string]string, len(count))}
	for p, n := range count {
		if n == 1 {
			idx.byPrefix[p] = owner[p]
		}
	}
	return idx
}

// Len is how many prefixes resolve, for logging and tests.
func (i Index) Len() int { return len(i.byPrefix) }

// Lookup resolves one hop. ok is false for a hop that is too short, unknown,
// or ambiguous — all three of which mean the same thing to a caller: this hop
// is not evidence about anybody.
func (i Index) Lookup(hop string) (key string, ok bool) {
	if len(hop) < prefixHex {
		return "", false
	}
	// A hop longer than the prefix is still usable: its first three bytes
	// identify the node just as well.
	k, ok := i.byPrefix[hop[:prefixHex]]
	return k, ok
}

// Attribute returns everything one packet proves.
//
// An advert yields its originator as sent, plus any relays as carried — a
// repeater re-broadcasting someone else's advert is carrying it, and that is
// what makes "adverts, including where it's in the path" work as a signal.
//
// Duplicate hops collapse: a route that lists the same node twice is one
// piece of evidence about that node, not two.
func Attribute(p corescope.Packet, idx Index) []Hit {
	var hits []Hit

	if p.PayloadType == corescope.TypeADVERT && len(p.AdvertPubKey) == 64 {
		hits = append(hits, Hit{Key: p.AdvertPubKey, Type: p.PayloadType, Direction: DirSent})
	}

	// A path only records where a packet HAS BEEN on a flood route, where
	// each relay appends its own hash. On a direct route the path is the
	// route the sender chose, and every hop removes itself before forwarding
	// — so a node still listed there is where the packet is GOING. Counting
	// that as "carried" credits a repeater for work it has not done, and on
	// the live mesh it accounted for 1,886 of 21,882 attributions, most of
	// them REQ packets on their way TO a repeater.
	//
	// TRACE is excluded outright: its path field holds SNR readings rather
	// than hashes ("append SNR (Not hash!)" in Mesh.cpp), so those bytes are
	// not identities at all and any match is coincidence.
	if !corescope.IsFloodRoute(p.RouteType) || p.PayloadType == corescope.TypeTRACE {
		return hits
	}

	var seen map[string]bool
	for _, hop := range p.PathHops {
		key, ok := idx.Lookup(hop)
		if !ok {
			continue
		}
		if seen == nil {
			seen = make(map[string]bool, len(p.PathHops))
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		hits = append(hits, Hit{Key: key, Type: p.PayloadType, Direction: DirCarried})
	}
	return hits
}
