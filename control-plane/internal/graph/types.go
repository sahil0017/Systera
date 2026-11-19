package graph

import (
	"fmt"
	"slices"
	"sort"
)

// Enumerations stored on nodes and edges (mirrors the database CHECK constraints).
var (
	LifecycleStates = []string{"PROPOSED", "ACTIVE", "DEPRECATED", "RETIRED"}
	EvidenceStates  = []string{"OBSERVED", "INFERRED", "PROPOSED"}
	EdgeDirections  = []string{"DIRECTED", "BIDIRECTIONAL"}
	SyncModes       = []string{"SYNC", "ASYNC", "BATCH", "STREAM"}
)

var (
	dataStores = []string{"database", "table", "cache", "queue", "topic"}
	runtimes   = []string{"service", "component"}
	allTypes   = NodeTypes
)

// relationRule constrains one relation type.
type relationRule struct {
	Sources, Targets []string
	// SameType requires source and target to have the same node type (replication,
	// failover between like resources).
	SameType bool
	Doc      string
}

// RelationRules is the edge compatibility matrix (checklist 2.2): which node types each
// relation may connect. depends_on and derived_from are deliberately generic.
var RelationRules = map[string]relationRule{
	"owns": {
		Sources: []string{"system", "domain", "service", "component"},
		Targets: []string{"domain", "service", "component", "api", "event", "topic", "database", "table",
			"cache", "queue", "slo", "test", "secret_boundary", "workload", "deployment_unit"},
		Doc: "ownership / containment",
	},
	"calls": {
		Sources: []string{"service", "component", "actor", "external_dependency", "workload", "test"},
		Targets: []string{"service", "component", "api", "external_dependency"},
		Doc:     "synchronous or asynchronous invocation",
	},
	"publishes": {
		Sources: []string{"service", "component", "external_dependency"},
		Targets: []string{"event", "topic", "queue"},
		Doc:     "emits messages to",
	},
	"subscribes": {
		Sources: []string{"service", "component", "external_dependency"},
		Targets: []string{"event", "topic", "queue"},
		Doc:     "consumes messages from",
	},
	"reads": {
		Sources: append(slices.Clone(runtimes), "external_dependency", "workload"),
		Targets: dataStores,
		Doc:     "reads data from",
	},
	"writes": {
		Sources: append(slices.Clone(runtimes), "external_dependency", "workload"),
		Targets: dataStores,
		Doc:     "writes data to",
	},
	"deploys_to": {
		Sources: []string{"service", "component", "api", "database", "cache", "queue", "topic", "deployment_unit"},
		Targets: []string{"deployment_unit", "region"},
		Doc:     "runs on / is placed in",
	},
	"depends_on": {Sources: allTypes, Targets: allTypes, Doc: "generic dependency"},
	"authenticates": {
		Sources: []string{"service", "component", "api", "external_dependency", "secret_boundary"},
		Targets: []string{"actor", "service", "component", "api"},
		Doc:     "verifies the identity of",
	},
	"replicates": {
		Sources: append(slices.Clone(dataStores), "region"), Targets: append(slices.Clone(dataStores), "region"),
		SameType: true, Doc: "copies data to a like resource",
	},
	"fails_over_to": {
		Sources:  []string{"service", "database", "cache", "queue", "topic", "region", "deployment_unit"},
		Targets:  []string{"service", "database", "cache", "queue", "topic", "region", "deployment_unit"},
		SameType: true, Doc: "standby for a like resource",
	},
	"derived_from": {Sources: allTypes, Targets: allTypes, Doc: "provenance (e.g. table derived from an entity)"},
}

// CheckRelation reports whether relation may connect a sourceType node to a targetType node.
func CheckRelation(relation, sourceType, targetType string) error {
	r, ok := RelationRules[relation]
	if !ok {
		return fmt.Errorf("unknown relation type %q", relation)
	}
	if !slices.Contains(r.Sources, sourceType) {
		return fmt.Errorf("%q edges cannot start at a %s (allowed: %v)", relation, sourceType, sorted(r.Sources))
	}
	if !slices.Contains(r.Targets, targetType) {
		return fmt.Errorf("%q edges cannot end at a %s (allowed: %v)", relation, targetType, sorted(r.Targets))
	}
	if r.SameType && sourceType != targetType {
		return fmt.Errorf("%q edges must connect resources of the same type (got %s -> %s)", relation, sourceType, targetType)
	}
	return nil
}

func sorted(in []string) []string {
	out := slices.Clone(in)
	sort.Strings(out)
	return out
}

func validNodeType(t string) bool     { return slices.Contains(NodeTypes, t) }
func validRelationType(t string) bool { return slices.Contains(RelationTypes, t) }
