package engineeringpulse

import (
	"fmt"
	"strings"
	"time"

	"github.com/systera/control-plane/internal/knowledge"
)

// GateConfig tunes the validation gate (2.7).
type GateConfig struct {
	MinConfidence  float64       // default 0.45
	MaxEvidenceAge time.Duration // newest evidence must be younger (default 365 days)
}

func (c GateConfig) withDefaults() GateConfig {
	if c.MinConfidence <= 0 {
		c.MinConfidence = 0.45
	}
	if c.MaxEvidenceAge <= 0 {
		c.MaxEvidenceAge = 365 * 24 * time.Hour
	}
	return c
}

// GateCheck is one named validation check.
type GateCheck struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail,omitempty"`
}

// GateResult is the validation outcome stored on the report. The database refuses a
// knowledge_update unless Passed is true.
type GateResult struct {
	Passed      bool        `json:"passed"`
	Checks      []GateCheck `json:"checks"`
	ValidatedAt time.Time   `json:"validated_at"`
}

// Failed lists the failing check names.
func (g GateResult) Failed() []string {
	var out []string
	for _, c := range g.Checks {
		if !c.Passed {
			out = append(out, c.Name)
		}
	}
	return out
}

// Validate runs the gate: evidence quality, claims, caveats and provenance must all pass
// before a report may become knowledge.
func Validate(r Report, now time.Time, cfg GateConfig) GateResult {
	cfg = cfg.withDefaults()
	res := GateResult{ValidatedAt: now.UTC()}
	add := func(name string, ok bool, format string, a ...any) {
		c := GateCheck{Name: name, Passed: ok}
		if !ok || format != "" {
			c.Detail = fmt.Sprintf(format, a...)
		}
		res.Checks = append(res.Checks, c)
	}
	ev := map[string]EvidenceItem{}
	sources := map[string]bool{}
	primary := 0
	for _, e := range r.Evidence {
		ev[e.ItemID] = e
		sources[e.SourceKey] = true
		if e.Role == "PRIMARY" && e.IsPrimarySource && e.SourceStatus == SourceApproved {
			primary++
		}
	}

	// 1. Evidence quality.
	add("primary_evidence", primary > 0, "%d primary-source item(s) from approved primary sources", primary)

	var notCanonical, unapproved []string
	for _, e := range r.Evidence {
		if !e.IsCanonical {
			notCanonical = append(notCanonical, e.ItemID)
		}
		if e.SourceStatus != SourceApproved {
			unapproved = append(unapproved, e.SourceKey)
		}
	}
	add("evidence_canonical", len(notCanonical) == 0, "duplicates cited: %v", notCanonical)
	add("sources_approved", len(unapproved) == 0, "unapproved sources: %v", sortedUnique(unapproved))

	// 2. Claims: every finding cites report evidence; technical proof must exist and be
	// backed by a primary source; adoption findings never stand alone.
	var uncited []int
	technicalPrimary, technical := 0, 0
	for i, f := range r.Findings {
		if len(f.Evidence) == 0 {
			uncited = append(uncited, i)
			continue
		}
		for _, id := range f.Evidence {
			if _, ok := ev[id]; !ok {
				uncited = append(uncited, i)
				break
			}
		}
		if f.Kind == FindingTechnical {
			technical++
			for _, id := range f.Evidence {
				if e, ok := ev[id]; ok && e.Role == "PRIMARY" {
					technicalPrimary++
					break
				}
			}
		} else if f.Kind != FindingAdoption {
			uncited = append(uncited, i)
		}
	}
	add("findings_cited", len(uncited) == 0 && len(r.Findings) > 0, "findings without valid citations: %v", uncited)
	add("technical_proof", technicalPrimary > 0, "%d technical finding(s), %d backed by a primary source", technical, technicalPrimary)

	// 3. Caveats & limitations.
	add("limitations_present", len(r.Limitations) > 0, "%d limitation(s)", len(r.Limitations))
	missing := []string{}
	if !contains(r.Caveats, CaveatAdoption) {
		missing = append(missing, "adoption")
	}
	if c, ok := maturityCaveat[r.Maturity]; ok && !contains(r.Caveats, c) {
		missing = append(missing, strings.ToLower(r.Maturity))
	}
	add("caveats_present", len(missing) == 0, "missing caveats: %v", missing)

	// 4. Confidence is calibrated and sufficient.
	wantBand := ConfidenceBand(r.Confidence, len(sources), primary)
	add("confidence_calibrated", r.ConfidenceBand == wantBand && r.Confidence >= 0 && r.Confidence <= 1,
		"band %s for confidence %.3f with %d source(s); expected %s", r.ConfidenceBand, r.Confidence, len(sources), wantBand)
	add("confidence_sufficient", r.Confidence >= cfg.MinConfidence, "confidence %.3f, minimum %.2f", r.Confidence, cfg.MinConfidence)

	// 5. Freshness.
	fresh := r.FreshnessAt != nil && now.Sub(*r.FreshnessAt) <= cfg.MaxEvidenceAge && r.StaleAfter.After(now)
	add("fresh", fresh, "newest evidence %s, stale after %s", fmtDate(r.FreshnessAt), r.StaleAfter.UTC().Format("2006-01-02"))

	// 6. Provenance: each cited item can be traced to an approved, documented source and a fetch.
	var noProv []string
	for _, e := range r.Evidence {
		if e.URL == "" || e.FetchID == "" || e.RetrievedAt.IsZero() || e.TermsURL == "" || e.Licence == "" {
			noProv = append(noProv, e.ItemID)
		}
	}
	add("provenance_complete", len(noProv) == 0 && len(r.Evidence) > 0, "items lacking url/fetch/terms/licence: %v", noProv)

	// 7. External content is untrusted: no instruction-override text may become knowledge.
	text := r.Title + "\n" + r.Summary
	for _, f := range r.Findings {
		text += "\n" + f.Claim
	}
	injected := knowledge.DetectPromptInjection(text)
	add("no_prompt_injection", !injected, "instruction-override pattern found in report text")

	res.Passed = true
	for _, c := range res.Checks {
		if !c.Passed {
			res.Passed = false
		}
	}
	return res
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
