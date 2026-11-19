package twin

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/systera/control-plane/internal/graph"
)

// Dimensions of the twin model (spec §23.4), in display order.
const (
	DimStructure  = "STRUCTURE"
	DimInterfaces = "INTERFACES"
	DimData       = "DATA"
	DimRuntime    = "RUNTIME"
	DimBehaviour  = "BEHAVIOUR"
	DimQuality    = "QUALITY_ATTRIBUTES"
)

var Dimensions = []string{DimStructure, DimInterfaces, DimData, DimRuntime, DimBehaviour, DimQuality}

// Check statuses and finding severities.
const (
	StatusPass = "PASS"
	StatusWarn = "WARN"
	StatusFail = "FAIL"
	StatusNA   = "NOT_APPLICABLE"

	SevError   = "ERROR"
	SevWarning = "WARNING"
	SevInfo    = "INFO"
)

// Report statuses.
const (
	ReportValid        = "VALID"
	ReportWithWarnings = "VALID_WITH_WARNINGS"
	ReportInvalid      = "INVALID"
)

// Report is the twin's validation report.
type Report struct {
	Status      string             `json:"status"`
	Basis       string             `json:"basis"`
	Disclaimer  string             `json:"disclaimer"`
	Errors      int                `json:"error_count"`
	Warnings    int                `json:"warning_count"`
	Dimensions  []DimensionSummary `json:"dimensions"`
	Checks      []Check            `json:"checks"`
	Limitations []string           `json:"limitations"`
}

type DimensionSummary struct {
	Dimension     string `json:"dimension"`
	Status        string `json:"status"`
	Elements      int    `json:"elements"`
	Passed        int    `json:"passed"`
	Warned        int    `json:"warned"`
	Failed        int    `json:"failed"`
	NotApplicable int    `json:"not_applicable"`
}

// Check is one deterministic rule; it passes or carries findings.
type Check struct {
	ID          string    `json:"id"`
	Dimension   string    `json:"dimension"`
	Title       string    `json:"title"`
	Status      string    `json:"status"`
	Detail      string    `json:"detail,omitempty"` // why NOT_APPLICABLE, or what passing means
	Findings    []Finding `json:"findings"`
	Remediation string    `json:"remediation,omitempty"`
}

// Finding is one problem a check found. ReliesOnDefaults is true when a builder default (not
// a declared or assumed value) decided the outcome.
type Finding struct {
	Severity         string    `json:"severity"`
	Message          string    `json:"message"`
	Subjects         []NodeRef `json:"subjects"`
	EdgeIDs          []string  `json:"edge_ids,omitempty"`
	ReliesOnDefaults bool      `json:"relies_on_defaults,omitempty"`
}

// Limitations every report states.
var Limitations = []string{
	"Contracts are referenced, not parsed: OpenAPI/AsyncAPI linting and breaking-change detection need contract artifacts and are not part of this build.",
	"Request paths follow declared calls only; asynchronous flows end at the publish boundary.",
	"The capacity check assumes every service on a request path sees the full stated peak load (no fan-out split, no caching).",
	"Values marked DEFAULT were filled in by the builder; declare them on the graph or in the assumptions to make results meaningful.",
}

type checker struct {
	t      Twin
	ix     *index
	checks []Check
}

func (c *checker) add(ch Check) {
	if ch.Findings == nil {
		ch.Findings = []Finding{}
	}
	if ch.Status == "" {
		ch.Status = StatusPass
		for _, f := range ch.Findings {
			switch f.Severity {
			case SevError:
				ch.Status = StatusFail
			case SevWarning:
				if ch.Status != StatusFail {
					ch.Status = StatusWarn
				}
			}
		}
	}
	c.checks = append(c.checks, ch)
}

// Validate runs every check against a twin built from g (the same graph the twin came from).
func Validate(t Twin, g graph.Graph) Report { return validate(t, newIndex(g)) }

func validate(t Twin, ix *index) Report {
	c := &checker{t: t, ix: ix}
	c.structure()
	c.interfaces()
	c.data()
	c.runtime()
	c.behaviour()
	c.quality()

	r := Report{Basis: Basis, Disclaimer: Disclaimer, Checks: c.checks, Limitations: slices.Clone(Limitations)}
	elements := map[string]int{
		DimStructure:  len(t.Structure.Components) + len(t.Structure.Boundaries),
		DimInterfaces: len(t.Interfaces),
		DimData:       len(t.Data),
		DimRuntime:    len(t.Runtime.Units),
		DimBehaviour:  len(t.Behaviour.Interactions),
		DimQuality:    len(t.Quality.SLOs),
	}
	for _, d := range Dimensions {
		s := DimensionSummary{Dimension: d, Elements: elements[d], Status: StatusPass}
		for _, ch := range c.checks {
			if ch.Dimension != d {
				continue
			}
			switch ch.Status {
			case StatusPass:
				s.Passed++
			case StatusWarn:
				s.Warned++
			case StatusFail:
				s.Failed++
			case StatusNA:
				s.NotApplicable++
			}
		}
		switch {
		case s.Failed > 0:
			s.Status = StatusFail
		case s.Warned > 0:
			s.Status = StatusWarn
		case s.Passed == 0:
			s.Status = StatusNA
		}
		r.Dimensions = append(r.Dimensions, s)
	}
	for _, ch := range c.checks {
		for _, f := range ch.Findings {
			switch f.Severity {
			case SevError:
				r.Errors++
			case SevWarning:
				r.Warnings++
			}
		}
	}
	switch {
	case r.Errors > 0:
		r.Status = ReportInvalid
	case r.Warnings > 0:
		r.Status = ReportWithWarnings
	default:
		r.Status = ReportValid
	}
	return r
}

func names(refs []NodeRef) string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.Name)
	}
	if len(out) > 6 {
		return strings.Join(out[:6], ", ") + fmt.Sprintf(" and %d more", len(out)-6)
	}
	return strings.Join(out, ", ")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// ---- STRUCTURE ----

var structuralTypes = []string{"service", "component", "api", "event", "topic", "queue", "database", "table", "cache", "external_dependency"}

func (c *checker) structure() {
	ix := c.ix

	// Orphans.
	var critical, plain []NodeRef
	for _, id := range ix.order {
		n := ix.nodes[id]
		if !slices.Contains(structuralTypes, n.Type) || ix.degree(id) > 0 {
			continue
		}
		if isCritical(n) {
			critical = append(critical, ix.ref(id))
		} else {
			plain = append(plain, ix.ref(id))
		}
	}
	ch := Check{ID: "STRUCT_ORPHANS", Dimension: DimStructure, Title: "No orphaned components",
		Remediation: "Connect the component to what calls it and what it uses, or remove it from the design."}
	if len(critical) > 0 {
		ch.Findings = append(ch.Findings, Finding{Severity: SevError, Subjects: critical,
			Message: fmt.Sprintf("%s marked critical but connected to nothing: %s.", plural(len(critical), "component is", "components are"), names(critical))})
	}
	if len(plain) > 0 {
		ch.Findings = append(ch.Findings, Finding{Severity: SevWarning, Subjects: plain,
			Message: fmt.Sprintf("%s no edges: %s.", plural(len(plain), "component has", "components have"), names(plain))})
	}
	c.add(ch)

	// Dependency cycles (calls through APIs, and depends_on).
	ch = Check{ID: "STRUCT_DEPENDENCY_CYCLES", Dimension: DimStructure, Title: "Dependency graph is acyclic",
		Remediation: "Break the cycle: invert one dependency with an event, extract the shared part, or merge the components."}
	for _, cyc := range c.cycles() {
		sev := SevWarning
		msg := "Dependency cycle: " + names(cyc.nodes) + "."
		if cyc.allSync {
			sev = SevError
			msg = "Synchronous call cycle (a request can wait on itself; retries amplify around the loop): " + names(cyc.nodes) + "."
		}
		ch.Findings = append(ch.Findings, Finding{Severity: sev, Message: msg, Subjects: cyc.nodes, EdgeIDs: cyc.edges})
	}
	c.add(ch)

	// Ownership.
	var unowned []NodeRef
	for _, comp := range c.t.Structure.Components {
		if (comp.Type == "service" || comp.Type == "component") && comp.OwnerSource == SourceMissing {
			unowned = append(unowned, comp.NodeRef)
		}
	}
	for _, d := range c.t.Data {
		n := ix.nodes[d.ID]
		if n.OwnerDomain == "" && len(ix.related(d.ID, false, "owns")) == 0 && d.Owner == nil {
			unowned = append(unowned, d.NodeRef)
		}
	}
	ch = Check{ID: "STRUCT_OWNERSHIP", Dimension: DimStructure, Title: "Every service and data store has an owner",
		Remediation: "Set owner_domain, or add an owns edge from the domain or service responsible."}
	if len(unowned) > 0 {
		ch.Findings = append(ch.Findings, Finding{Severity: SevWarning, Subjects: unowned,
			Message: fmt.Sprintf("%s no owner: %s.", plural(len(unowned), "element has", "elements have"), names(unowned))})
	}
	c.add(ch)

	// Lifecycle: live elements depending on deprecated or retired ones.
	ch = Check{ID: "STRUCT_LIFECYCLE", Dimension: DimStructure, Title: "Nothing live depends on deprecated or retired parts",
		Remediation: "Migrate the dependency, or record an ADR with a retirement date."}
	for _, id := range ix.order {
		n := ix.nodes[id]
		if n.LifecycleState == "DEPRECATED" || n.LifecycleState == "RETIRED" {
			continue
		}
		for _, e := range ix.out[id] {
			t := ix.nodes[e.TargetNodeID]
			if (t.LifecycleState == "DEPRECATED" || t.LifecycleState == "RETIRED") && e.RelationType != "derived_from" && e.RelationType != "owns" {
				sev := SevWarning
				if t.LifecycleState == "RETIRED" {
					sev = SevError
				}
				ch.Findings = append(ch.Findings, Finding{Severity: sev, Subjects: []NodeRef{ix.ref(id), ix.ref(t.ID)}, EdgeIDs: []string{e.ID},
					Message: fmt.Sprintf("%s %s %s, which is %s.", n.Name, e.RelationType, t.Name, strings.ToLower(t.LifecycleState))})
			}
		}
	}
	c.add(ch)
}

type cycle struct {
	nodes   []NodeRef
	edges   []string
	allSync bool
}

// cycles finds strongly connected components (Tarjan) over calls (resolved through APIs to
// their providers) and depends_on edges.
func (c *checker) cycles() []cycle {
	ix := c.ix
	type arc struct {
		to, edge string
		sync     bool
	}
	adj := map[string][]arc{}
	syncOf := map[string]bool{}
	for _, it := range c.t.Behaviour.Interactions {
		syncOf[it.EdgeID] = isSyncCall(it)
	}
	for _, id := range ix.order {
		for _, e := range ix.out[id] {
			switch e.RelationType {
			case "calls":
				to := []string{e.TargetNodeID}
				if ix.nodes[e.TargetNodeID].Type == "api" {
					if p := ix.providerOf(e.TargetNodeID); len(p) > 0 {
						to = p
					}
				}
				for _, t := range to {
					adj[id] = append(adj[id], arc{t, e.ID, syncOf[e.ID]})
				}
			case "depends_on":
				adj[id] = append(adj[id], arc{e.TargetNodeID, e.ID, false})
			}
		}
	}
	idx, low := map[string]int{}, map[string]int{}
	onStack := map[string]bool{}
	var stack []string
	n := 0
	var out []cycle
	var strong func(v string)
	strong = func(v string) {
		idx[v], low[v] = n, n
		n++
		stack = append(stack, v)
		onStack[v] = true
		for _, a := range adj[v] {
			if _, seen := idx[a.to]; !seen {
				strong(a.to)
				low[v] = min(low[v], low[a.to])
			} else if onStack[a.to] {
				low[v] = min(low[v], idx[a.to])
			}
		}
		if low[v] != idx[v] {
			return
		}
		var members []string
		for {
			w := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			onStack[w] = false
			members = append(members, w)
			if w == v {
				break
			}
		}
		in := map[string]bool{}
		for _, m := range members {
			in[m] = true
		}
		cy := cycle{allSync: true}
		for _, m := range members {
			for _, a := range adj[m] {
				if in[a.to] {
					cy.edges = append(cy.edges, a.edge)
					cy.allSync = cy.allSync && a.sync
				}
			}
		}
		if len(members) == 1 && len(cy.edges) == 0 {
			return // not a cycle
		}
		sort.Strings(members)
		for _, m := range members {
			cy.nodes = append(cy.nodes, ix.ref(m))
		}
		sort.Slice(cy.nodes, func(i, j int) bool { return cy.nodes[i].Key < cy.nodes[j].Key })
		sort.Strings(cy.edges)
		cy.edges = slices.Compact(cy.edges)
		out = append(out, cy)
	}
	for _, id := range ix.order {
		if _, seen := idx[id]; !seen {
			strong(id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].nodes[0].Key < out[j].nodes[0].Key })
	return out
}

// ---- INTERFACES ----

func (c *checker) interfaces() {
	ifs := c.t.Interfaces

	ch := Check{ID: "IFACE_PROVIDER", Dimension: DimInterfaces, Title: "Every API has a provider",
		Remediation: "Add an owns edge from the service that implements the API."}
	var apis, orphanAPIs []NodeRef
	for _, it := range ifs {
		if it.Kind == "API" {
			apis = append(apis, it.NodeRef)
			if len(it.Providers) == 0 {
				orphanAPIs = append(orphanAPIs, it.NodeRef)
			}
		}
	}
	if len(apis) == 0 {
		ch.Status, ch.Detail = StatusNA, "The design has no API nodes."
	} else if len(orphanAPIs) > 0 {
		ch.Findings = append(ch.Findings, Finding{Severity: SevWarning, Subjects: orphanAPIs,
			Message: fmt.Sprintf("%s no implementing service: %s.", plural(len(orphanAPIs), "API has", "APIs have"), names(orphanAPIs))})
	}
	c.add(ch)

	ch = Check{ID: "IFACE_CHANNEL_ENDS", Dimension: DimInterfaces, Title: "Events and channels have both producers and consumers",
		Remediation: "Add the missing publisher or subscriber, or remove the channel."}
	channels := 0
	var noPub, noSub []NodeRef
	for _, it := range ifs {
		if it.Kind == "API" {
			continue
		}
		channels++
		if len(it.Providers) == 0 {
			noPub = append(noPub, it.NodeRef)
		}
		if len(it.Consumers) == 0 {
			noSub = append(noSub, it.NodeRef)
		}
	}
	if channels == 0 {
		ch.Status, ch.Detail = StatusNA, "The design has no events, topics or queues."
	}
	if len(noPub) > 0 {
		ch.Findings = append(ch.Findings, Finding{Severity: SevWarning, Subjects: noPub,
			Message: fmt.Sprintf("Nothing publishes to %s.", names(noPub))})
	}
	if len(noSub) > 0 {
		ch.Findings = append(ch.Findings, Finding{Severity: SevWarning, Subjects: noSub,
			Message: fmt.Sprintf("Nothing consumes %s; messages accumulate or are lost.", names(noSub))})
	}
	c.add(ch)

	ch = Check{ID: "IFACE_CONTRACTS", Dimension: DimInterfaces, Title: "Interfaces declare a versioned contract",
		Remediation: "Set version and schema_ref (an OpenAPI/AsyncAPI/JSON Schema reference) on the interface node."}
	var undeclared, partial []NodeRef
	for _, it := range ifs {
		switch it.ContractBasis {
		case "UNDECLARED":
			undeclared = append(undeclared, it.NodeRef)
		case "SCHEMA_ONLY", "VERSION_ONLY":
			partial = append(partial, it.NodeRef)
		}
	}
	if len(ifs) == 0 {
		ch.Status, ch.Detail = StatusNA, "The design has no interfaces."
	}
	if len(undeclared) > 0 {
		ch.Findings = append(ch.Findings, Finding{Severity: SevWarning, Subjects: undeclared,
			Message: fmt.Sprintf("%s no version or schema, so breaking changes cannot be detected: %s.", plural(len(undeclared), "interface has", "interfaces have"), names(undeclared))})
	}
	if len(partial) > 0 {
		ch.Findings = append(ch.Findings, Finding{Severity: SevInfo, Subjects: partial,
			Message: fmt.Sprintf("Only a version or only a schema is declared for %s.", names(partial))})
	}
	c.add(ch)

	ch = Check{ID: "IFACE_PROTOCOLS", Dimension: DimInterfaces, Title: "Calls declare a protocol",
		Remediation: "Set protocol on the call edge (HTTPS, gRPC, …)."}
	var noProto []NodeRef
	var noProtoEdges []string
	calls := 0
	for _, it := range c.t.Behaviour.Interactions {
		if it.Relation != "calls" {
			continue
		}
		calls++
		if it.Protocol == "" {
			noProto = append(noProto, it.From)
			noProtoEdges = append(noProtoEdges, it.EdgeID)
		}
	}
	if calls == 0 {
		ch.Status, ch.Detail = StatusNA, "The design has no calls."
	} else if len(noProtoEdges) > 0 {
		ch.Findings = append(ch.Findings, Finding{Severity: SevInfo, Subjects: dedupeRefs(noProto), EdgeIDs: noProtoEdges,
			Message: fmt.Sprintf("%s no protocol.", plural(len(noProtoEdges), "call has", "calls have"))})
	}
	c.add(ch)
}

func dedupeRefs(in []NodeRef) []NodeRef {
	seen := map[string]bool{}
	out := []NodeRef{}
	for _, r := range in {
		if !seen[r.ID] {
			seen[r.ID] = true
			out = append(out, r)
		}
	}
	return out
}

// ---- DATA ----

func (c *checker) data() {
	ix := c.ix
	stores := c.t.Data
	na := func(ch Check) Check {
		if len(stores) == 0 {
			ch.Status, ch.Detail = StatusNA, "The design has no databases, tables or caches."
		}
		return ch
	}

	ch := na(Check{ID: "DATA_SINGLE_WRITER", Dimension: DimData, Title: "Each database has one writing service",
		Remediation: "Give the data one owning service and have others go through its API or events."})
	for _, d := range stores {
		if d.Type == "cache" {
			continue
		}
		var runtimeWriters []NodeRef
		for _, w := range d.Writers {
			if w.Type == "service" || w.Type == "component" {
				runtimeWriters = append(runtimeWriters, w)
			}
		}
		if len(runtimeWriters) > 1 {
			ch.Findings = append(ch.Findings, Finding{Severity: SevWarning, Subjects: append([]NodeRef{d.NodeRef}, runtimeWriters...),
				Message: fmt.Sprintf("%s is written by %d services (%s): shared ownership couples their schemas and deployments.", d.Name, len(runtimeWriters), names(runtimeWriters))})
		}
	}
	c.add(ch)

	ch = na(Check{ID: "DATA_ACCESS", Dimension: DimData, Title: "Data stores are both written and read",
		Remediation: "Add the writer or reader edge, or remove the store."})
	for _, d := range stores {
		switch {
		case len(d.Writers) == 0 && len(d.Readers) > 0 && d.Type != "cache":
			ch.Findings = append(ch.Findings, Finding{Severity: SevWarning, Subjects: []NodeRef{d.NodeRef},
				Message: fmt.Sprintf("%s is read but nothing writes it.", d.Name)})
		case len(d.Readers) == 0 && len(d.Writers) > 0:
			ch.Findings = append(ch.Findings, Finding{Severity: SevInfo, Subjects: []NodeRef{d.NodeRef},
				Message: fmt.Sprintf("%s is written but nothing in the design reads it.", d.Name)})
		}
	}
	c.add(ch)

	ch = na(Check{ID: "DATA_CACHE_BACKING", Dimension: DimData, Title: "Caches have a system of record",
		Remediation: "Show where cache misses go: the reader should also read the database the cache fronts."})
	caches := 0
	for _, d := range stores {
		if d.Type != "cache" {
			continue
		}
		caches++
		backed := false
		for _, r := range append(slices.Clone(d.Readers), d.Writers...) {
			for _, e := range ix.out[r.ID] {
				if (e.RelationType == "reads" || e.RelationType == "writes") && slices.Contains([]string{"database", "table"}, ix.nodes[e.TargetNodeID].Type) {
					backed = true
				}
			}
		}
		if !backed {
			ch.Findings = append(ch.Findings, Finding{Severity: SevWarning, Subjects: []NodeRef{d.NodeRef},
				Message: fmt.Sprintf("Nothing that uses %s also reads a database; a cache loss would lose the data.", d.Name)})
		}
	}
	if caches == 0 && len(stores) > 0 {
		ch.Status, ch.Detail = StatusNA, "The design has no caches."
	}
	c.add(ch)

	ch = na(Check{ID: "DATA_SEMANTICS", Dimension: DimData, Title: "Databases declare consistency and retention",
		Remediation: "Set consistency (e.g. strong, read-committed, eventual) and retention on the database node."})
	var missing []NodeRef
	for _, d := range stores {
		if d.Type != "cache" && (d.Consistency == "" || d.Retention == "") {
			missing = append(missing, d.NodeRef)
		}
	}
	if len(missing) > 0 {
		ch.Findings = append(ch.Findings, Finding{Severity: SevInfo, Subjects: missing,
			Message: fmt.Sprintf("Consistency or retention is undeclared for %s.", names(missing))})
	}
	c.add(ch)
}

// ---- RUNTIME ----

func (c *checker) runtime() {
	rt := c.t.Runtime
	q := c.t.Quality
	onPath := map[string]bool{}
	for _, p := range c.t.Behaviour.Paths {
		for _, n := range p.Nodes {
			onPath[n.ID] = true
		}
	}
	coveredAvail := c.availabilityByNode()

	ch := Check{ID: "RUNTIME_PLACEMENT", Dimension: DimRuntime, Title: "Runtime units are placed on a deployment unit or region",
		Remediation: "Add deploys_to edges from services and stores to deployment units or regions."}
	var unplaced []NodeRef
	for _, u := range rt.Units {
		if len(u.PlacedOn) == 0 {
			unplaced = append(unplaced, u.NodeRef)
		}
	}
	switch {
	case len(rt.Units) == 0:
		ch.Status, ch.Detail = StatusNA, "The design has no services or stores."
	case !rt.TopologyModeled:
		ch.Findings = append(ch.Findings, Finding{Severity: SevInfo, Subjects: []NodeRef{}, ReliesOnDefaults: true,
			Message: "Deployment topology is not modelled (no deployment units or regions); runtime checks use the environment assumptions."})
	case len(unplaced) > 0:
		ch.Findings = append(ch.Findings, Finding{Severity: SevWarning, Subjects: unplaced,
			Message: fmt.Sprintf("%s not placed anywhere: %s.", plural(len(unplaced), "unit is", "units are"), names(unplaced))})
	}
	c.add(ch)

	ch = Check{ID: "RUNTIME_REDUNDANCY", Dimension: DimRuntime, Title: "Services on request paths run more than one replica",
		Remediation: "Declare replicas ≥ 2 on the service or its deployment unit (or raise environment.default_replicas)."}
	for _, u := range rt.Units {
		if (u.Type != "service" && u.Type != "component") || u.Replicas > 1 {
			continue
		}
		critical := isCritical(c.ix.nodes[u.ID])
		if !onPath[u.ID] && !critical {
			continue
		}
		sev := SevWarning
		msg := fmt.Sprintf("%s runs a single replica (%s); losing it stops every request through it.", u.Name, strings.ToLower(u.ReplicasSource))
		if a := coveredAvail[u.ID]; a >= 99.9 {
			sev = SevError
			msg = fmt.Sprintf("%s runs a single replica (%s) but is covered by a %.2f%% availability objective.", u.Name, strings.ToLower(u.ReplicasSource), a)
		}
		ch.Findings = append(ch.Findings, Finding{Severity: sev, Message: msg, Subjects: []NodeRef{u.NodeRef}, ReliesOnDefaults: u.ReplicasSource == SourceDefault})
	}
	c.add(ch)

	ch = Check{ID: "RUNTIME_REGIONS", Dimension: DimRuntime, Title: "Region count matches the availability and recovery objectives",
		Remediation: "Add a second region (and fails_over_to), or relax the objective with an ADR."}
	switch {
	case q.TargetAvail == 0 && !q.HasRecoveryTo:
		ch.Status, ch.Detail = StatusNA, "No availability or RTO/RPO objective is declared."
	case len(rt.Regions) <= 1 && q.TargetAvail >= 99.95:
		region := "an unknown region"
		if len(rt.Regions) == 1 {
			region = "one region (" + rt.Regions[0] + ")"
		}
		ch.Findings = append(ch.Findings, Finding{Severity: SevWarning, Subjects: []NodeRef{}, ReliesOnDefaults: rt.RegionsSource == SourceMissing,
			Message: fmt.Sprintf("The strictest objective is %.2f%% availability, but the design runs in %s; a regional outage breaks it.", q.TargetAvail, region)})
	default:
		ch.Detail = fmt.Sprintf("%s for a strictest objective of %.2f%%.", plural(len(rt.Regions), "region", "regions"), q.TargetAvail)
	}
	c.add(ch)

	ch = Check{ID: "RUNTIME_DATA_REDUNDANCY", Dimension: DimRuntime, Title: "Databases have a replica or failover when recovery objectives exist",
		Remediation: "Add replicates or fails_over_to from the database to a like resource."}
	dbs := 0
	for _, d := range c.t.Data {
		if d.Type != "database" {
			continue
		}
		dbs++
		// A database that is itself a replica or standby is the redundancy, not a gap.
		isStandby := len(c.ix.related(d.ID, false, "replicates", "fails_over_to")) > 0
		if !isStandby && len(d.Replicas) == 0 && len(d.FailoverTo) == 0 && (q.HasRecoveryTo || coveredAvail[d.ID] >= 99.9) {
			ch.Findings = append(ch.Findings, Finding{Severity: SevWarning, Subjects: []NodeRef{d.NodeRef},
				Message: fmt.Sprintf("%s has no replica or failover target; RPO/RTO depend on restoring from backup.", d.Name)})
		}
	}
	if dbs == 0 {
		ch.Status, ch.Detail = StatusNA, "The design has no databases."
	} else if !q.HasRecoveryTo && len(ch.Findings) == 0 {
		ch.Status, ch.Detail = StatusNA, "No RTO/RPO objective is declared."
	}
	c.add(ch)

	ch = Check{ID: "RUNTIME_CAPACITY", Dimension: DimRuntime, Title: "Declared capacity carries the stated peak load",
		Remediation: "Raise replicas, declare capacity_rps per replica, or revise workload.peak_rps."}
	peak := c.t.Assumptions.Workload.PeakRPS
	if peak <= 0 {
		ch.Status, ch.Detail = StatusNA, "No workload.peak_rps assumption; capacity engines need one."
	} else {
		var unknown []NodeRef
		checked := 0
		for _, u := range rt.Units {
			if (u.Type != "service" && u.Type != "component") || !onPath[u.ID] {
				continue
			}
			if u.CapacityRPS == 0 {
				unknown = append(unknown, u.NodeRef)
				continue
			}
			checked++
			if total := u.CapacityRPS * float64(u.Replicas); total < peak {
				ch.Findings = append(ch.Findings, Finding{Severity: SevError, Subjects: []NodeRef{u.NodeRef}, ReliesOnDefaults: u.ReplicasSource == SourceDefault,
					Message: fmt.Sprintf("%s: %d × %.0f rps = %.0f rps, below the %.0f rps peak.", u.Name, u.Replicas, u.CapacityRPS, total, peak)})
			}
		}
		if len(unknown) > 0 {
			ch.Findings = append(ch.Findings, Finding{Severity: SevInfo, Subjects: unknown,
				Message: fmt.Sprintf("No capacity_rps declared for %s; the capacity engine will need resource profiles.", names(unknown))})
		}
		if checked == 0 && len(ch.Findings) == len(unknown) && len(unknown) > 0 {
			ch.Status, ch.Detail = StatusNA, "No service on a request path declares capacity_rps."
		}
	}
	c.add(ch)
}

// availabilityByNode returns, per covered node, the strictest availability objective covering it.
func (c *checker) availabilityByNode() map[string]float64 {
	out := map[string]float64{}
	for _, s := range c.t.Quality.SLOs {
		for _, n := range s.Covers {
			out[n.ID] = max(out[n.ID], s.AvailabilityPct)
		}
	}
	return out
}

// ---- BEHAVIOUR ----

func (c *checker) behaviour() {
	b := c.t.Behaviour
	var sync []Interaction
	for _, it := range b.Interactions {
		if isSyncCall(it) {
			sync = append(sync, it)
		}
	}
	noSync := len(sync) == 0

	ch := Check{ID: "BEHAV_TIMEOUTS", Dimension: DimBehaviour, Title: "Synchronous calls declare timeouts",
		Remediation: "Set timeout_ms on the call edge (constraints), or environment.default_timeout_ms."}
	if noSync {
		ch.Status, ch.Detail = StatusNA, "The design has no synchronous calls."
	}
	var extNo, intNo []Interaction
	for _, it := range sync {
		if it.TimeoutMS > 0 {
			continue
		}
		if it.To.Type == "external_dependency" {
			extNo = append(extNo, it)
		} else {
			intNo = append(intNo, it)
		}
	}
	if len(extNo) > 0 {
		ch.Findings = append(ch.Findings, Finding{Severity: SevError, Subjects: interactionRefs(extNo), EdgeIDs: interactionEdges(extNo),
			Message: fmt.Sprintf("%s to external dependencies %s no timeout; a slow provider ties up callers indefinitely.", plural(len(extNo), "call", "calls"), hasHave(len(extNo)))})
	}
	if len(intNo) > 0 {
		ch.Findings = append(ch.Findings, Finding{Severity: SevWarning, Subjects: interactionRefs(intNo), EdgeIDs: interactionEdges(intNo),
			Message: fmt.Sprintf("%s %s no timeout.", plural(len(intNo), "internal call", "internal calls"), hasHave(len(intNo)))})
	}
	c.add(ch)

	ch = Check{ID: "BEHAV_RETRY_POLICY", Dimension: DimBehaviour, Title: "Retries are bounded by a timeout and use backoff",
		Remediation: "Pair every retry with a timeout and exponential backoff with jitter; add a circuit breaker on hot paths."}
	retrying := 0
	for _, it := range sync {
		if it.Retries == 0 {
			continue
		}
		retrying++
		if it.TimeoutMS == 0 {
			ch.Findings = append(ch.Findings, Finding{Severity: SevError, Subjects: []NodeRef{it.From, it.To}, EdgeIDs: []string{it.EdgeID},
				Message: fmt.Sprintf("%s → %s retries %d× with no timeout.", it.From.Name, it.To.Name, it.Retries)})
		} else if !it.Backoff {
			ch.Findings = append(ch.Findings, Finding{Severity: SevWarning, Subjects: []NodeRef{it.From, it.To}, EdgeIDs: []string{it.EdgeID},
				Message: fmt.Sprintf("%s → %s retries %d× without backoff; failures turn into synchronized retry waves.", it.From.Name, it.To.Name, it.Retries)})
		}
	}
	if noSync {
		ch.Status, ch.Detail = StatusNA, "The design has no synchronous calls."
	} else if retrying == 0 {
		ch.Detail = "No call declares retries."
	}
	c.add(ch)

	ch = Check{ID: "BEHAV_RETRY_AMPLIFICATION", Dimension: DimBehaviour, Title: "Nested retries do not amplify load",
		Remediation: "Retry at one layer only (usually the edge), and fail fast below it."}
	if len(b.Paths) == 0 {
		ch.Status, ch.Detail = StatusNA, "No request paths could be derived."
	}
	seenAmp := map[string]bool{}
	for _, p := range b.Paths {
		if p.RetryAmplification <= 4 {
			continue
		}
		key := strings.Join(p.EdgeIDs, ",")
		if seenAmp[key] {
			continue
		}
		seenAmp[key] = true
		sev := SevWarning
		if p.RetryAmplification > 10 {
			sev = SevError
		}
		ch.Findings = append(ch.Findings, Finding{Severity: sev, Subjects: p.Nodes, EdgeIDs: p.EdgeIDs,
			Message: fmt.Sprintf("One request from %s can become %d calls at the deepest hop (%s).", p.Entry.Name, p.RetryAmplification, pathText(p))})
	}
	c.add(ch)

	ch = Check{ID: "BEHAV_TIMEOUT_ORDER", Dimension: DimBehaviour, Title: "Inner timeouts are shorter than outer ones",
		Remediation: "Give each downstream call (including its retries) less time than the call that waits on it."}
	byEdge := map[string]Interaction{}
	for _, it := range b.Interactions {
		byEdge[it.EdgeID] = it
	}
	pairs := 0
	seenPair := map[string]bool{}
	for _, p := range b.Paths {
		var prev *Interaction
		for _, eid := range p.EdgeIDs {
			it, ok := byEdge[eid]
			if !ok || !isSyncCall(it) {
				prev = nil
				continue
			}
			if prev != nil && prev.TimeoutMS > 0 && it.TimeoutMS > 0 {
				pairs++
				inner := it.TimeoutMS * (it.Retries + 1)
				key := prev.EdgeID + ">" + it.EdgeID
				if inner >= prev.TimeoutMS && !seenPair[key] {
					seenPair[key] = true
					ch.Findings = append(ch.Findings, Finding{Severity: SevWarning, Subjects: []NodeRef{prev.From, it.From, it.To}, EdgeIDs: []string{prev.EdgeID, it.EdgeID},
						Message: fmt.Sprintf("%s waits %d ms for %s, which can spend %d ms on %s; the outer call times out first and the inner work is wasted.",
							prev.From.Name, prev.TimeoutMS, it.From.Name, inner, it.To.Name)})
				}
			}
			cur := it
			prev = &cur
		}
	}
	if pairs == 0 && len(ch.Findings) == 0 {
		ch.Status, ch.Detail = StatusNA, "No consecutive synchronous calls with declared timeouts."
	}
	c.add(ch)

	ch = Check{ID: "BEHAV_SYNC_DEPTH", Dimension: DimBehaviour, Title: "Synchronous chains stay shallow",
		Remediation: "Collapse hops, cache, or move work off the request path with events."}
	if len(b.Paths) == 0 {
		ch.Status, ch.Detail = StatusNA, "No request paths could be derived."
	}
	seenDeep := map[string]bool{}
	for _, p := range b.Paths {
		if p.SyncHops > 4 {
			key := strings.Join(p.EdgeIDs, ",")
			if !seenDeep[key] {
				seenDeep[key] = true
				ch.Findings = append(ch.Findings, Finding{Severity: SevWarning, Subjects: p.Nodes, EdgeIDs: p.EdgeIDs,
					Message: fmt.Sprintf("%d synchronous hops from %s: %s.", p.SyncHops, p.Entry.Name, pathText(p))})
			}
		}
	}
	if b.PathsCapped {
		ch.Findings = append(ch.Findings, Finding{Severity: SevInfo, Subjects: []NodeRef{},
			Message: fmt.Sprintf("Request path enumeration stopped at %d paths; some paths were not analysed.", maxPaths)})
	}
	c.add(ch)

	ch = Check{ID: "BEHAV_EVENT_LOOPS", Dimension: DimBehaviour, Title: "No service consumes its own events",
		Remediation: "Use an internal call or state machine instead, or document why the loop terminates."}
	pubs := map[string]map[string]bool{}
	for _, it := range b.Interactions {
		if it.Relation == "publishes" {
			if pubs[it.From.ID] == nil {
				pubs[it.From.ID] = map[string]bool{}
			}
			pubs[it.From.ID][it.To.ID] = true
		}
	}
	for _, it := range b.Interactions {
		if it.Relation == "subscribes" && pubs[it.From.ID][it.To.ID] {
			ch.Findings = append(ch.Findings, Finding{Severity: SevInfo, Subjects: []NodeRef{it.From, it.To}, EdgeIDs: []string{it.EdgeID},
				Message: fmt.Sprintf("%s publishes to and consumes %s.", it.From.Name, it.To.Name)})
		}
	}
	if len(pubs) == 0 {
		ch.Status, ch.Detail = StatusNA, "Nothing publishes events."
	}
	c.add(ch)
}

func hasHave(n int) string {
	if n == 1 {
		return "has"
	}
	return "have"
}

func interactionRefs(its []Interaction) []NodeRef {
	var out []NodeRef
	for _, it := range its {
		out = append(out, it.From, it.To)
	}
	return dedupeRefs(out)
}

func interactionEdges(its []Interaction) []string {
	out := make([]string, 0, len(its))
	for _, it := range its {
		out = append(out, it.EdgeID)
	}
	return out
}

func pathText(p RequestPath) string {
	parts := make([]string, 0, len(p.Nodes))
	for _, n := range p.Nodes {
		parts = append(parts, n.Name)
	}
	return strings.Join(parts, " → ")
}

// ---- QUALITY ATTRIBUTES ----

func (c *checker) quality() {
	q := c.t.Quality
	b := c.t.Behaviour

	ch := Check{ID: "QUAL_SLOS_DEFINED", Dimension: DimQuality, Title: "Service level objectives are defined and attached",
		Remediation: "Add slo nodes with availability_pct / latency_p99_ms and link them to the APIs or services they govern."}
	if len(q.SLOs) == 0 {
		ch.Findings = append(ch.Findings, Finding{Severity: SevWarning, Subjects: []NodeRef{},
			Message: "The design declares no SLOs, so nothing defines what “working” means for simulation or review."})
	}
	for _, s := range q.SLOs {
		if len(s.Covers) == 0 {
			ch.Findings = append(ch.Findings, Finding{Severity: SevWarning, Subjects: []NodeRef{s.NodeRef},
				Message: fmt.Sprintf("SLO %s is not linked to anything.", s.Name)})
		}
		if s.AvailabilityPct == 0 && s.LatencyP95MS == 0 && s.LatencyP99MS == 0 && s.RTOMinutes == 0 && s.RPOMinutes == 0 {
			ch.Findings = append(ch.Findings, Finding{Severity: SevWarning, Subjects: []NodeRef{s.NodeRef},
				Message: fmt.Sprintf("SLO %s has no numeric target.", s.Name)})
		}
		if s.AvailabilityPct > 100 {
			ch.Findings = append(ch.Findings, Finding{Severity: SevError, Subjects: []NodeRef{s.NodeRef},
				Message: fmt.Sprintf("SLO %s targets %.2f%% availability, which is impossible.", s.Name, s.AvailabilityPct)})
		}
	}
	c.add(ch)

	ch = Check{ID: "QUAL_PATH_COVERAGE", Dimension: DimQuality, Title: "Every request path is governed by an SLO",
		Remediation: "Attach an SLO to each entry API (or the service behind it)."}
	if q.PathsTotal == 0 {
		ch.Status, ch.Detail = StatusNA, "No request paths could be derived."
	} else {
		ch.Detail = fmt.Sprintf("%d of %d request paths covered (%.1f%%).", q.PathsCovered, q.PathsTotal, q.CoveragePct)
		uncovered := map[string]NodeRef{}
		for _, p := range b.Paths {
			if len(p.CoveredBy) == 0 {
				uncovered[p.Entry.ID] = p.Entry
			}
		}
		if len(uncovered) > 0 {
			refs := make([]NodeRef, 0, len(uncovered))
			for _, r := range uncovered {
				refs = append(refs, r)
			}
			sort.Slice(refs, func(i, j int) bool { return refs[i].Key < refs[j].Key })
			ch.Findings = append(ch.Findings, Finding{Severity: SevWarning, Subjects: refs,
				Message: fmt.Sprintf("%d of %d request paths have no SLO (entry points: %s).", q.PathsTotal-q.PathsCovered, q.PathsTotal, names(refs))})
		}
	}
	c.add(ch)

	ch = Check{ID: "QUAL_LATENCY_BUDGET", Dimension: DimQuality, Title: "Worst-case timeouts fit the latency objective",
		Remediation: "Lower inner timeouts or retries, or revise the latency objective."}
	slo := map[string]SLO{}
	for _, s := range q.SLOs {
		slo[s.ID] = s
	}
	compared := 0
	seen := map[string]bool{}
	for _, p := range b.Paths {
		if p.TimeoutBudgetMS == 0 {
			continue
		}
		for _, sid := range p.CoveredBy {
			s := slo[sid]
			target := s.LatencyP99MS
			if target == 0 {
				target = s.LatencyP95MS
			}
			if target == 0 {
				continue
			}
			compared++
			key := sid + "|" + strings.Join(p.EdgeIDs, ",")
			if float64(p.TimeoutBudgetMS) > target && !seen[key] {
				seen[key] = true
				ch.Findings = append(ch.Findings, Finding{Severity: SevWarning, Subjects: append([]NodeRef{s.NodeRef}, p.Nodes...), EdgeIDs: p.EdgeIDs,
					Message: fmt.Sprintf("Worst case on %s is %d ms of timeouts and retries, above the %.0f ms objective of %s. Slow failures will breach the SLO before they are cut off.",
						pathText(p), p.TimeoutBudgetMS, target, s.Name)})
			}
		}
	}
	if compared == 0 {
		ch.Status, ch.Detail = StatusNA, "Needs a latency SLO on a path whose calls all declare timeouts."
	}
	c.add(ch)

	ch = Check{ID: "QUAL_RECOVERY_OBJECTIVES", Dimension: DimQuality, Title: "Recovery objectives (RTO/RPO) are stated for stored data",
		Remediation: "Add rto_minutes and rpo_minutes to an SLO linked to the databases."}
	hasDB := false
	for _, d := range c.t.Data {
		hasDB = hasDB || d.Type == "database" || d.Type == "table"
	}
	if !hasDB {
		ch.Status, ch.Detail = StatusNA, "The design stores no data."
	} else if !q.HasRecoveryTo {
		ch.Findings = append(ch.Findings, Finding{Severity: SevInfo, Subjects: []NodeRef{},
			Message: "No SLO states an RTO or RPO, so failure simulation cannot judge data-loss or recovery time."})
	}
	c.add(ch)
}
