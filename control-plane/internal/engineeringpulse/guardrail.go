package engineeringpulse

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// Guardrail (2.9, §6.5): a trend score never selects architecture technology. Options are
// ranked on engineering criteria only; the trend score may break a near-tie and is reported
// separately as an adoption signal.

// Criteria every option must be assessed on.
var Criteria = []string{"maturity", "operational_burden", "cost", "security", "fit"}

// TieBreakMargin: options whose engineering scores differ by less than this many points
// (0-100) count as tied, and only then may the trend score order them.
const TieBreakMargin = 2.0

// OptionAssessment is one candidate technology, assessed by an agent or a person.
type OptionAssessment struct {
	Name              string            `json:"name"`
	Maturity          string            `json:"maturity"`           // EXPERIMENTAL..DEPRECATED
	OperationalBurden int               `json:"operational_burden"` // 1 (light) .. 5 (heavy)
	Cost              int               `json:"cost"`               // 1 (cheap) .. 5 (expensive)
	Security          int               `json:"security"`           // 1 (weak) .. 5 (strong)
	Fit               int               `json:"fit"`                // 1 (poor) .. 5 (excellent) fit to requirements
	Rationale         map[string]string `json:"rationale"`          // one entry per criterion
	EvidenceRefs      []string          `json:"evidence_refs,omitempty"`
	TrendScore        *float64          `json:"trend_score,omitempty"` // adoption signal only
}

// CriteriaWeights weights the engineering criteria (defaults: fit .30, security .20,
// maturity .20, operational burden .15, cost .15).
type CriteriaWeights struct {
	Fit, Security, Maturity, OperationalBurden, Cost float64
}

// DefaultCriteriaWeights are used when none are given.
var DefaultCriteriaWeights = CriteriaWeights{Fit: 0.30, Security: 0.20, Maturity: 0.20, OperationalBurden: 0.15, Cost: 0.15}

// RankedOption is one option's result.
type RankedOption struct {
	Rank             int                `json:"rank"`
	Name             string             `json:"name"`
	EngineeringScore float64            `json:"engineering_score"` // 0-100, trend-independent
	CriterionScores  map[string]float64 `json:"criterion_scores"`  // each 0-100
	TrendScore       *float64           `json:"trend_score,omitempty"`
	TieBrokenByTrend bool               `json:"tie_broken_by_trend,omitempty"`
	Excluded         bool               `json:"excluded,omitempty"`
	ExclusionReason  string             `json:"exclusion_reason,omitempty"`
}

// Comparison is the guardrailed result.
type Comparison struct {
	Recommended    string             `json:"recommended"`
	SelectionBasis string             `json:"selection_basis"`
	TrendScoreRole string             `json:"trend_score_role"`
	Weights        map[string]float64 `json:"weights"`
	Options        []RankedOption     `json:"options"`
	Warnings       []string           `json:"warnings,omitempty"`
}

var maturityPoints = map[string]float64{
	MaturityMainstream: 100, MaturityEmerging: 70, MaturityLegacy: 45, MaturityExperimental: 35, MaturityDeprecated: 0,
}

// CompareOptions ranks options by maturity, operational burden, cost, security and fit.
// It refuses to rank when any option lacks a criterion or its rationale, so a trend score can
// never stand in for the assessment.
func CompareOptions(opts []OptionAssessment, w *CriteriaWeights) (Comparison, error) {
	if len(opts) < 2 {
		return Comparison{}, fmt.Errorf("%w: compare at least two options", ErrInvalidInput)
	}
	weights := DefaultCriteriaWeights
	if w != nil {
		weights = *w
	}
	sum := weights.Fit + weights.Security + weights.Maturity + weights.OperationalBurden + weights.Cost
	if sum <= 0 || weights.Fit < 0 || weights.Security < 0 || weights.Maturity < 0 || weights.OperationalBurden < 0 || weights.Cost < 0 {
		return Comparison{}, fmt.Errorf("%w: criteria weights must be non-negative and not all zero", ErrInvalidInput)
	}
	for _, x := range []float64{weights.Fit, weights.Security, weights.Maturity, weights.OperationalBurden, weights.Cost} {
		if x == 0 {
			return Comparison{}, fmt.Errorf("%w: every engineering criterion must carry weight", ErrInvalidInput)
		}
	}

	var problems []string
	seen := map[string]bool{}
	for _, o := range opts {
		name := strings.TrimSpace(o.Name)
		if name == "" {
			problems = append(problems, "an option has no name")
			continue
		}
		if seen[strings.ToLower(name)] {
			problems = append(problems, fmt.Sprintf("%s: duplicate option", name))
		}
		seen[strings.ToLower(name)] = true
		if _, ok := maturityPoints[o.Maturity]; !ok {
			problems = append(problems, fmt.Sprintf("%s: maturity must be one of EXPERIMENTAL, EMERGING, MAINSTREAM, LEGACY, DEPRECATED", name))
		}
		for crit, v := range map[string]int{"operational_burden": o.OperationalBurden, "cost": o.Cost, "security": o.Security, "fit": o.Fit} {
			if v < 1 || v > 5 {
				problems = append(problems, fmt.Sprintf("%s: %s must be rated 1-5", name, crit))
			}
		}
		for _, crit := range Criteria {
			if strings.TrimSpace(o.Rationale[crit]) == "" {
				problems = append(problems, fmt.Sprintf("%s: rationale for %s is required", name, crit))
			}
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return Comparison{}, fmt.Errorf("%w: %s", ErrIncompleteCriteria, strings.Join(problems, "; "))
	}

	scale := func(v int) float64 { return float64(v-1) / 4 * 100 } // 1..5 -> 0..100
	ranked := make([]RankedOption, len(opts))
	for i, o := range opts {
		cs := map[string]float64{
			"maturity":           maturityPoints[o.Maturity],
			"operational_burden": 100 - scale(o.OperationalBurden),
			"cost":               100 - scale(o.Cost),
			"security":           scale(o.Security),
			"fit":                scale(o.Fit),
		}
		es := (weights.Fit*cs["fit"] + weights.Security*cs["security"] + weights.Maturity*cs["maturity"] +
			weights.OperationalBurden*cs["operational_burden"] + weights.Cost*cs["cost"]) / sum
		ranked[i] = RankedOption{Name: o.Name, EngineeringScore: round2(es), CriterionScores: cs, TrendScore: o.TrendScore}
		if o.Maturity == MaturityDeprecated {
			ranked[i].Excluded, ranked[i].ExclusionReason = true, "deprecated technology is not recommended for new designs"
		}
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		a, b := ranked[i], ranked[j]
		if a.Excluded != b.Excluded {
			return !a.Excluded
		}
		return a.EngineeringScore > b.EngineeringScore
	})
	// Trend score may only reorder adjacent near-ties.
	for i := 0; i+1 < len(ranked); i++ {
		a, b := ranked[i], ranked[i+1]
		if a.Excluded || b.Excluded || math.Abs(a.EngineeringScore-b.EngineeringScore) >= TieBreakMargin {
			continue
		}
		if ts(b) > ts(a) {
			ranked[i], ranked[i+1] = b, a
			ranked[i].TieBrokenByTrend = true
		}
	}
	cmp := Comparison{
		SelectionBasis: "engineering_criteria",
		TrendScoreRole: fmt.Sprintf("tie_break_only (engineering scores within %.0f points)", TieBreakMargin),
		Weights: map[string]float64{"fit": weights.Fit / sum, "security": weights.Security / sum, "maturity": weights.Maturity / sum,
			"operational_burden": weights.OperationalBurden / sum, "cost": weights.Cost / sum},
	}
	for i := range ranked {
		ranked[i].Rank = i + 1
	}
	cmp.Options = ranked
	if !ranked[0].Excluded {
		cmp.Recommended = ranked[0].Name
	} else {
		cmp.Warnings = append(cmp.Warnings, "every option is excluded; no recommendation")
	}

	// Explain where attention and engineering merit disagree.
	var topTrend *RankedOption
	for i := range ranked {
		if ranked[i].TrendScore != nil && (topTrend == nil || *ranked[i].TrendScore > *topTrend.TrendScore) {
			topTrend = &ranked[i]
		}
	}
	if topTrend != nil && topTrend.Rank != 1 {
		cmp.Warnings = append(cmp.Warnings, fmt.Sprintf("%s has the highest trend score (%.1f) but ranks #%d on engineering criteria; attention is not fitness.",
			topTrend.Name, *topTrend.TrendScore, topTrend.Rank))
	}
	return cmp, nil
}

func ts(o RankedOption) float64 {
	if o.TrendScore == nil {
		return -1
	}
	return *o.TrendScore
}
