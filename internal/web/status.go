package web

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"time"

	"strings"

	"hopreact/internal/corescope"
	"hopreact/internal/health"
)

// The public status board.
//
// One verdict per repeater, not one per signal. A repeater is judged on the
// most recent thing seen of it — an advert or ordinary traffic — so either
// arriving puts it back to green. Two nodes both silent for a day are in the
// same state whether the last thing heard was a channel message or a beacon,
// and splitting that across columns made the reader do arithmetic to find out
// if anything was wrong.
//
// What the two signals ARE still matters, so it moves into the hover: the
// board shows a colour and an age, and explains itself when you ask.

// statusState is one repeater's verdict.
type statusState struct {
	Class string // good | amber | orange | bad | muted
	Label string
	// Since is the most recent evidence of any kind.
	Since time.Time
	// Known is false when nothing has ever been recorded, which is not the
	// same as silence: per-type evidence covers roughly four packets in ten,
	// so some repeaters simply never produce anything we can pin to them.
	Known bool
	// Why is the hover text — which signal was last, and what the colour means.
	Why string
}

type statusRow struct {
	Name string
	Key  string
	// Link points at more detail: a node's CoreScope page, or a service's
	// own address.
	Link   string
	Status statusState
	// Detail is a short qualifier shown beside the name — a backbone
	// repeater's scores, or an observer's role.
	Detail string
	rank   int
}

// statusGroup is one section of the board. Grouping matters because the
// failures mean different things: a broker being down stops every observer
// reporting at once, whereas one backbone repeater going quiet splits the
// mesh, and an ordinary repeater going quiet is a Tuesday.
type statusGroup struct {
	Name string
	Note string
	Rows []statusRow
	// Critical marks a group whose failure degrades everything downstream, so
	// the overall verdict weighs it more heavily.
	Critical bool

	OK, Quiet, Concern, Late, Unknown, Total int
}

// State is the group's own verdict, driving its heading colour.
func (g statusGroup) State() string {
	switch {
	case g.Total == 0:
		return "muted"
	case g.Late > 0:
		return "bad"
	case g.Concern > 0:
		return "orange"
	case g.Quiet > 0:
		return "amber"
	}
	return "good"
}

// Summary is the group's one-line verdict.
func (g statusGroup) Summary() string {
	switch {
	case g.Total == 0:
		return "nothing to report"
	case g.Late > 0:
		return fmt.Sprintf("%d of %d down", g.Late, g.Total)
	case g.Concern > 0:
		return fmt.Sprintf("%d of %d need a look", g.Concern, g.Total)
	case g.Quiet > 0:
		return fmt.Sprintf("%d of %d quiet", g.Quiet, g.Total)
	}
	return fmt.Sprintf("all %d operational", g.Total)
}

func (g *statusGroup) tally(r statusRow) {
	g.Rows = append(g.Rows, r)
	g.Total++
	switch r.Status.Class {
	case "bad":
		g.Late++
	case "orange":
		g.Concern++
	case "amber":
		g.Quiet++
	case "muted":
		g.Unknown++
	default:
		g.OK++
	}
}

type statusPage struct {
	Title      string
	Region     string
	Now        time.Time
	Groups     []statusGroup
	Recent     []statusRow
	OK         int
	Quiet      int
	Concern    int
	Late       int
	Unknown    int
	Total      int
	StaleDays  int
	QuietHrs   int
	ConcernHrs int
	AlarmHrs   int
	RecentHrs  int
	Updated    time.Time
	FeedOK     bool
	FeedReason string
	// Verdict is the whole-system headline: operational, degraded, or a
	// partial outage.
	Verdict string
	Class   string
	// CoreScopeURL is the instance node names link into.
	CoreScopeURL string
}

// Pct is a band's share of the bar.
func (p statusPage) Pct(n int) float64 {
	if p.Total == 0 {
		return 0
	}
	return float64(n) / float64(p.Total) * 100
}

// AllWell reports whether there is nothing to report, which is the headline a
// status board should be able to give in three words.
func (p statusPage) AllWell() bool {
	return p.Total > 0 && p.Late == 0 && p.Concern == 0 && p.Quiet == 0
}

// judge produces the verdict for one repeater.
//
// The clock runs from whichever signal is fresher. An advert proves the box
// is powered and announcing itself; carried traffic proves it is doing its
// job. Either is enough to call it alive, so either resets the colour.
func judge(lastTraffic, lastAdvert, now time.Time, quiet, concern, alarm int) statusState {
	newest := lastTraffic
	if lastAdvert.After(newest) {
		newest = lastAdvert
	}
	if newest.IsZero() {
		return statusState{Class: "muted", Label: "no data", Known: false,
			Why: "Nothing has ever been attributed to this repeater. A node is only identifiable in a route when that route uses hashes of three bytes or more — about four packets in ten — so this means we cannot see it, not that it is down."}
	}

	age := now.Sub(newest)
	st := statusState{Since: newest, Known: true}
	switch {
	case age >= time.Duration(alarm)*time.Hour:
		st.Class, st.Label = "bad", "not active"
	case age >= time.Duration(concern)*time.Hour:
		st.Class, st.Label = "orange", "concerning"
	case age >= time.Duration(quiet)*time.Hour:
		st.Class, st.Label = "amber", "a bit quiet"
	default:
		st.Class, st.Label = "good", "ok"
	}

	// Say which signal is carrying the verdict, and what would change it.
	var parts string
	switch {
	case lastAdvert.IsZero():
		parts = fmt.Sprintf("Last seen carrying traffic %s. It has never been heard advertising itself.",
			humanSince(lastTraffic, now))
	case lastTraffic.IsZero():
		parts = fmt.Sprintf("Last advertised itself %s. It has never been seen carrying traffic we could attribute.",
			humanSince(lastAdvert, now))
	default:
		parts = fmt.Sprintf("Last advertised itself %s; last seen carrying traffic %s.",
			humanSince(lastAdvert, now), humanSince(lastTraffic, now))
	}
	st.Why = parts + " " + meaning(st.Class, quiet, concern, alarm)
	return st
}

func meaning(class string, quiet, concern, alarm int) string {
	switch class {
	case "good":
		return fmt.Sprintf("Anything within %dh counts as fine.", quiet)
	case "amber":
		return fmt.Sprintf("Quiet for over %dh. Often just a quiet mesh or a thin patch of listener coverage rather than a fault.", quiet)
	case "orange":
		return fmt.Sprintf("Nothing for over %dh. Worth asking about — a working repeater usually advertises itself well inside this.", concern)
	}
	return fmt.Sprintf("Nothing at all for over %dh. A repeater beacons on its own schedule regardless of whether anyone is using the mesh, so this one has almost certainly stopped.", alarm)
}

// handleStatus renders the public board. No sign-in: it is a notice board for
// a community's shared infrastructure, and requiring an account to see
// whether the mesh is up would defeat the point.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if !s.Cfg.Status.Enabled {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	now := s.Store.Now().UTC()
	c := s.Cfg.Status

	std, _ := templateByID("standard")
	targets, err := s.Store.StatusTargets(ctx, std.Types)
	if err != nil {
		s.Log.Error("loading status targets", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	page := statusPage{
		Title: c.Title, Region: s.regionName(), Now: now,
		StaleDays: c.StaleAfterDays,
		QuietHrs:  c.QuietHours, ConcernHrs: c.ConcernHours,
		AlarmHrs: c.AlarmHours, RecentHrs: c.RecentHours,
	}
	page.CoreScopeURL = strings.TrimRight(s.Cfg.CoreScope.APIURL, "/")
	fh := s.feedHealth(r)
	page.FeedOK, page.FeedReason, page.Updated = fh.Healthy, fh.Reason, fh.LastOK

	services := statusGroup{
		Name: "Observer services", Critical: true,
		Note: "The infrastructure everything else reports through. If one of these is down, the mesh may be fine while this page cannot tell.",
	}
	var probes []health.Result
	if s.Health != nil {
		probes = s.Health.Results()
	}
	for _, res := range probes {
		st := statusState{Known: true, Since: res.At, Why: res.Note}
		if res.OK {
			st.Class, st.Label = "good", "operational"
		} else {
			st.Class, st.Label = "bad", "unreachable"
		}
		if res.Detail != "" {
			st.Why = strings.TrimSpace(st.Why + " Last probe: " + res.Detail + ".")
		}
		detail := res.Target
		if res.OK && res.Latency > 0 {
			detail = fmt.Sprintf("%s · %dms", res.Target, res.Latency.Milliseconds())
		}
		// Link to the service itself, not to whatever endpoint we happen to
		// probe: /api/observers and /healthz are machine addresses, and a
		// reader clicking a service name wants the thing, not its pulse.
		link := ""
		if res.Kind != "tcp" {
			if u, err := url.Parse(res.Target); err == nil && u.Host != "" {
				link = u.Scheme + "://" + u.Host
			}
		}
		services.tally(statusRow{Name: res.Name, Detail: detail, Status: st, Link: link})
	}

	core := statusGroup{
		Name: "Core repeaters", Critical: true,
		Note: fmt.Sprintf("The backbone. Either a bridge score over %.0f%% — how many routes across the mesh run through it — or a traffic share over %.0f%%. Losing one of these takes other repeaters with it.",
			c.CoreBridgeScore*100, c.CoreTrafficShare*100),
	}
	repeaters := statusGroup{
		Name: "Repeaters",
		Note: "Everything else carrying traffic in the region.",
	}
	observers := statusGroup{
		Name: "Observers",
		Note: "The stations listening to the mesh and reporting what they hear. These are judged on whether they check in, not on what they have heard — a quiet patch of mesh is not a broken listener.",
	}

	// Anything not seen for this long is dropped rather than shown as a
	// permanent red. A status board is for things that are supposed to be
	// working; a repeater nobody expects back is not an incident, it is
	// history. CoreScope stops reporting nodes at seven days anyway.
	var staleBefore time.Time
	if c.StaleAfterDays > 0 {
		staleBefore = now.AddDate(0, 0, -c.StaleAfterDays)
	}
	seenWithin := func(times ...time.Time) bool {
		if staleBefore.IsZero() {
			return true
		}
		for _, ts := range times {
			if ts.After(staleBefore) {
				return true
			}
		}
		return false
	}

	for _, t := range targets {
		if t.Kind == "observer" {
			if !seenWithin(t.LastSeen, t.LastPacket) {
				continue
			}
			// An observer is judged on its own check-in. What it has heard
			// depends on how busy the mesh around it is, not on whether it
			// works.
			st := judge(t.LastSeen, time.Time{}, now, c.QuietHours, c.ConcernHours, c.AlarmHours)
			if st.Known {
				st.Why = "Last checked in " + humanSince(t.LastSeen, now) + ". " +
					meaning(st.Class, c.QuietHours, c.ConcernHours, c.AlarmHours)
			}
			row := statusRow{Name: t.Name, Key: t.Key, Status: st}
			if row.Name == "" {
				row.Name = shortKey(t.Key)
			}
			if !t.LastPacket.IsZero() {
				row.Detail = "last heard traffic " + humanSince(t.LastPacket, now)
			}
			row.Link = nodeLink(page.CoreScopeURL, t.Key)
			row.rank = rankOf(st.Class)
			observers.tally(row)
			continue
		}

		// Outside the region, or advertising no location at all. On a bridged
		// mesh this is most of them — 556 of 659 repeaters on the live feed
		// are Irish, Manx or English — and listing those would make this
		// somebody else's status board.
		if !s.Region.Empty() {
			if t.Lat == nil || t.Lon == nil || (*t.Lat == 0 && *t.Lon == 0) {
				continue
			}
			if !s.Region.Contains(*t.Lat, *t.Lon) {
				continue
			}
		}
		if !seenWithin(t.LastSeen, t.LastStandard, t.LastAdvert) {
			continue
		}
		row := statusRow{Name: t.Name, Key: t.Key,
			Status: judge(t.LastStandard, t.LastAdvert, now,
				c.QuietHours, c.ConcernHours, c.AlarmHours)}
		if row.Name == "" {
			row.Name = shortKey(t.Key)
		}
		row.rank = rankOf(row.Status.Class)
		row.Link = nodeLink(page.CoreScopeURL, t.Key)

		pinned := isPinnedCore(t.Key, c.CoreKeys)
		scores := t.BridgeScore >= c.CoreBridgeScore || t.TrafficShare >= c.CoreTrafficShare
		// Still backbone if it was recently, even though the scores have
		// lapsed — which is exactly what happens when one goes down.
		recently := c.CoreGraceDays > 0 && !t.LastCoreAt.IsZero() &&
			t.LastCoreAt.After(now.AddDate(0, 0, -c.CoreGraceDays))

		if pinned || scores || recently {
			row.Detail = fmt.Sprintf("bridge %.0f%% · traffic %.0f%%",
				t.BridgeScore*100, t.TrafficShare*100)
			switch {
			case pinned:
				row.Detail += " · marked core"
			case !scores && recently:
				// Say so, or a reader wonders why a node scoring nothing is
				// listed as backbone.
				row.Detail += " · backbone until " +
					stamp(t.LastCoreAt.AddDate(0, 0, c.CoreGraceDays))
			}
			core.tally(row)
		} else {
			repeaters.tally(row)
		}
	}

	for _, g := range []statusGroup{services, core, repeaters, observers} {
		if g.Total == 0 && len(g.Rows) == 0 && g.Name == "Observer services" {
			continue // nothing configured; do not show an empty section
		}
		sort.SliceStable(g.Rows, func(i, j int) bool {
			if g.Rows[i].rank != g.Rows[j].rank {
				return g.Rows[i].rank > g.Rows[j].rank
			}
			return g.Rows[i].Name < g.Rows[j].Name
		})
		page.Groups = append(page.Groups, g)
		page.OK += g.OK
		page.Quiet += g.Quiet
		page.Concern += g.Concern
		page.Late += g.Late
		page.Unknown += g.Unknown
		page.Total += g.Total
	}

	// The whole-system headline. A failure in a critical group is an outage;
	// anywhere else it is degradation, because one quiet repeater among
	// seventy is not the mesh being down.
	page.Verdict, page.Class = verdict(page.Groups)

	// What changed recently, across every group.
	cutoff := now.Add(-time.Duration(c.RecentHours) * time.Hour)
	for _, g := range page.Groups {
		for _, row := range g.Rows {
			if len(page.Recent) >= c.RecentLimit {
				break
			}
			if row.Status.Known && row.Status.Class != "good" && row.Status.Since.After(cutoff) {
				page.Recent = append(page.Recent, row)
			}
		}
	}

	d := s.page(r, page.Title)
	d.Status = &page
	s.render(w, "status.html", d)
}

// nodeLink points at a node's page on the CoreScope instance this board is
// built from, so a name is a way through to the detail rather than a dead
// end. Empty when there is no instance configured.
func nodeLink(base, key string) string {
	if base == "" || key == "" {
		return ""
	}
	return base + "/#/nodes/" + url.PathEscape(key)
}

// isPinnedCore reports whether an operator has named this repeater as
// backbone regardless of its scores.
func isPinnedCore(key string, pinned []string) bool {
	k := strings.ToLower(key)
	for _, p := range pinned {
		p = strings.ToLower(strings.TrimSpace(p))
		if p != "" && strings.HasPrefix(k, p) {
			return true
		}
	}
	return false
}

// verdict turns the groups into one headline.
//
// Only the critical groups — the services everything reports through, and
// the backbone repeaters — can put the mesh into an outage. Everything else
// is degradation. Seventy repeaters means one is nearly always having a bad
// day, and a board that calls that an outage is a board people stop reading.
func verdict(groups []statusGroup) (string, string) {
	var criticalDown, criticalWarn, otherTrouble bool
	total := 0
	for _, g := range groups {
		total += g.Total
		trouble := g.Late > 0
		warn := g.Concern > 0 || g.Quiet > 0
		switch {
		case g.Critical && trouble:
			criticalDown = true
		case g.Critical && warn:
			criticalWarn = true
		case trouble || warn:
			otherTrouble = true
		}
	}
	switch {
	case total == 0:
		return "Nothing to report yet", "muted"
	case criticalDown:
		return "Major outage", "bad"
	case criticalWarn:
		return "Partial outage", "orange"
	case otherTrouble:
		return "Degraded", "amber"
	}
	return "All systems operational", "good"
}

// rankOf sorts trouble to the top, with never-seen last: an unknown is not an
// outage and should not crowd one out.
func rankOf(class string) int {
	switch class {
	case "bad":
		return 4
	case "orange":
		return 3
	case "amber":
		return 2
	case "good":
		return 1
	}
	return 0
}

func (s *Server) regionName() string {
	if s.Cfg.Status.RegionName != "" {
		return s.Cfg.Status.RegionName
	}
	if s.Region != nil {
		return s.Region.Name
	}
	return ""
}

// statusTypeNames lists what counts as traffic, for the page's own
// explanation of itself.
func statusTypeNames() []string {
	std, _ := templateByID("standard")
	out := make([]string, 0, len(std.Types))
	for _, t := range std.Types {
		out = append(out, corescope.TypeName(t))
	}
	return out
}
