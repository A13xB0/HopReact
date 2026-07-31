package store

import "hopreact/internal/corescope"

// RuleTemplate is a ready-made rule offered as one click, so the common case
// doesn't mean ticking ten boxes.
//
// This is the single source of truth for what a watch starts with AND what
// the watch page offers afterwards. It used to live in the web layer only,
// which meant a freshly added watch was seeded with a different, untuned set
// of rules than the "ready-made rules" menu would ever produce — the two
// drifted, and the drift was visible. Applying one stores the expanded types
// like any other rule, so changing a template never rewrites what someone is
// already being alerted on.
type RuleTemplate struct {
	ID   string
	Name string
	Note string
	// Kind restricts a template to one sort of target. An observer has no
	// packet types to watch and a node has no check-in, so offering either
	// template on the wrong page would just be a control that cannot work.
	Kind           string
	Source         Source
	Types          []int
	Direction      Direction
	ThresholdHours int
}

// RuleTemplates are the templates on offer, in display order. The first
// matching a kind is that kind's default for a new watch.
var RuleTemplates = []RuleTemplate{
	{
		ID:     "standard-observer",
		Name:   "Standard observer",
		Kind:   string(corescope.KindObserver),
		Source: SourceSeen,
		Note: "Alerts if the station hasn't checked in for an hour — its last status on CoreScope. " +
			"This is the right question for an observer: whether it has HEARD anything depends on how busy the mesh around it is, not on whether it's working.",
		Direction:      DirEither,
		ThresholdHours: 1,
	},
	{
		ID:   "standard",
		Kind: string(corescope.KindNode),
		Name: "Standard",
		Note: "Everything a repeater either sends itself or genuinely carries, at 12 hours. Leaves out requests aimed AT the repeater, traces (which never identify one), and neighbour discovery (which never travels far enough to be heard).",
		Types: []int{
			corescope.TypeRESPONSE, corescope.TypeTXTMsg, corescope.TypeACK,
			corescope.TypeADVERT, corescope.TypeGRPTXT, corescope.TypePATH,
		},
		Direction:      DirEither,
		ThresholdHours: 12,
	},
	{
		ID:   "adverts",
		Kind: string(corescope.KindNode),
		Name: "Adverts only",
		Note: "The most reliable heartbeat: a node states its own key in an advert, so this never depends on route hashes. " +
			"A day and an hour, so a node that adverts daily gets some slack before this complains.",
		Types:          []int{corescope.TypeADVERT},
		Direction:      DirEither,
		ThresholdHours: 25,
	},
	{
		ID:   "traffic",
		Kind: string(corescope.KindNode),
		Name: "Carrying traffic",
		Note: "Only counts messages this node passed on for other people, which is the job a repeater is actually there to do.",
		Types: []int{
			corescope.TypeTXTMsg, corescope.TypeGRPTXT,
			corescope.TypeGRPData, corescope.TypeMULTIPART,
		},
		Direction:      DirCarried,
		ThresholdHours: 12,
	},
}

// TemplateByID finds one template.
func TemplateByID(id string) (RuleTemplate, bool) {
	for _, t := range RuleTemplates {
		if t.ID == id {
			return t, true
		}
	}
	return RuleTemplate{}, false
}

// TemplatesFor returns the templates that make sense for one kind of target.
func TemplatesFor(kind string) []RuleTemplate {
	var out []RuleTemplate
	for _, t := range RuleTemplates {
		if t.Kind == "" || t.Kind == kind {
			out = append(out, t)
		}
	}
	return out
}

// DefaultTemplateID is the template a new watch starts from when nothing was
// chosen — the first on offer for its kind.
func DefaultTemplateID(kind string) string {
	for _, t := range RuleTemplates {
		if t.Kind == "" || t.Kind == kind {
			return t.ID
		}
	}
	return ""
}

// Rule expands a template into the rule it stands for. The copy matters:
// rules are mutated after expansion (thresholds overridden, types sorted),
// and sharing the template's slice would let one saved rule quietly edit the
// template every later watch is seeded from.
func (t RuleTemplate) Rule() Rule {
	src := t.Source
	if src == "" {
		src = SourceTypes
	}
	return Rule{
		Label:          t.Name,
		Source:         src,
		Types:          append([]int(nil), t.Types...),
		Direction:      t.Direction,
		ThresholdHours: t.ThresholdHours,
	}
}
