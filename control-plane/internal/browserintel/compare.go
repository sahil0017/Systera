package browserintel

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/systera/control-plane/internal/platform/ids"
)

// Compare websites (§7.6): two or more user-selected website investigations compared on the
// same capability taxonomy. Comparison reads only investigations the actor can already see
// and never collects anything new.

// Comparison limits.
const (
	MinCompareSites = 2
	MaxCompareSites = 5
)

// CompareDisclaimer accompanies every comparison.
const CompareDisclaimer = "Compares what was visible on the investigated pages of each site. A capability missing from a site may simply not have been on the pages you selected; this is not a comparison of private backends."

// CompareInput selects the investigations to compare.
type CompareInput struct {
	WebsiteIDs []string `json:"website_ids"`
}

// ComparedSite summarises one investigation in a comparison.
type ComparedSite struct {
	WebsiteID    string       `json:"website_id"`
	Domain       string       `json:"domain"`
	SessionID    string       `json:"session_id"`
	AnalysisMode AnalysisMode `json:"analysis_mode"`
	PageCount    int          `json:"page_count"`
	Status       string       `json:"status"`
	ModelID      string       `json:"model_id,omitempty"`
}

// CapabilityCell is one site's entry for one taxonomy capability.
type CapabilityCell struct {
	Present       bool   `json:"present"`
	EvidenceClass string `json:"evidence_class,omitempty"`
	Confidence    string `json:"confidence,omitempty"`
	PageCount     int    `json:"page_count"`
}

// CapabilityRow compares one taxonomy capability across sites.
type CapabilityRow struct {
	Key               string                    `json:"key"`
	Name              string                    `json:"name"`
	ProposedComponent string                    `json:"proposed_component"`
	Sites             map[string]CapabilityCell `json:"sites"` // website_id -> cell
	PresentOn         int                       `json:"present_on"`
}

// TechnologyRow compares one passive technology signal across sites.
type TechnologyRow struct {
	Name  string            `json:"name"`
	Sites map[string]string `json:"sites"` // website_id -> confidence (absent = not seen)
}

// SitePair is the pairwise similarity of two sites on the taxonomy.
type SitePair struct {
	A                    string   `json:"a"`
	B                    string   `json:"b"`
	CapabilitySimilarity float64  `json:"capability_similarity"` // Jaccard over observed capabilities, 0..1
	SharedCapabilities   []string `json:"shared_capabilities"`
	SharedTechnology     []string `json:"shared_technology"`
}

// WebsiteComparison is the result of comparing investigations.
type WebsiteComparison struct {
	TaxonomyVersion string              `json:"taxonomy_version"`
	Disclaimer      string              `json:"disclaimer"`
	Sites           []ComparedSite      `json:"sites"`
	Capabilities    []CapabilityRow     `json:"capabilities"`
	Shared          []string            `json:"shared_capabilities"`
	Unique          map[string][]string `json:"unique_capabilities"`   // website_id -> names
	Unmapped        map[string][]string `json:"unmapped_capabilities"` // agent findings outside the taxonomy
	Journeys        map[string][]string `json:"journeys"`
	Technology      []TechnologyRow     `json:"technology"`
	Pairs           []SitePair          `json:"pairs"`
	Notes           []string            `json:"notes"`
}

type compareSource struct {
	site     ComparedSite
	profile  WebsiteProfile
	findings []Finding
}

// CompareWebsites compares 2-5 of the actor's investigations on the shared capability taxonomy.
func (s *Store) CompareWebsites(ctx context.Context, tenantID string, a Actor, in CompareInput) (WebsiteComparison, error) {
	idsIn := dedupeStrings(in.WebsiteIDs)
	if len(idsIn) < MinCompareSites || len(idsIn) > MaxCompareSites {
		return WebsiteComparison{}, fmt.Errorf("%w: compare between %d and %d distinct investigations", ErrInvalidInput, MinCompareSites, MaxCompareSites)
	}
	for _, id := range idsIn {
		if !ids.Valid(id) {
			return WebsiteComparison{}, fmt.Errorf("%w: website_ids must be UUIDs", ErrInvalidInput)
		}
	}
	var srcs []compareSource
	err := s.tx(ctx, tenantID, func(tx pgx.Tx) error {
		for _, id := range idsIn {
			w, err := websiteTx(ctx, tx, id, a, true, false)
			if err != nil {
				return err
			}
			sess, err := scanSession(tx.QueryRow(ctx, `SELECT `+sessionCols+` FROM browser_sessions WHERE id = $1`, w.SessionID))
			if err != nil {
				return err
			}
			f, err := findingsTx(ctx, tx, w.ID, "website_capabilities")
			if err != nil {
				return err
			}
			srcs = append(srcs, compareSource{
				site: ComparedSite{WebsiteID: w.ID, Domain: w.Domain, SessionID: w.SessionID, AnalysisMode: sess.AnalysisMode,
					PageCount: len(w.SourceObservationIDs), Status: w.Status, ModelID: w.ModelID},
				profile: w, findings: f,
			})
		}
		return nil
	})
	if err != nil {
		return WebsiteComparison{}, err
	}
	return BuildComparison(srcsToInputs(srcs)), nil
}

// ComparisonInput is one site's data for BuildComparison.
type ComparisonInput struct {
	Site     ComparedSite
	Profile  WebsiteProfile
	Findings []Finding // website_capabilities rows (baseline and agent)
}

func srcsToInputs(srcs []compareSource) []ComparisonInput {
	out := make([]ComparisonInput, len(srcs))
	for i, s := range srcs {
		out[i] = ComparisonInput{Site: s.site, Profile: s.profile, Findings: s.findings}
	}
	return out
}

var taxonomyByName = func() map[string]TaxonomyEntry {
	m := map[string]TaxonomyEntry{}
	for _, t := range CapabilityTaxonomy {
		m[strings.ToLower(t.Name)] = t
	}
	return m
}()

// BuildComparison compares sites on the shared capability taxonomy (pure; used by
// CompareWebsites and tests).
func BuildComparison(in []ComparisonInput) WebsiteComparison {
	out := WebsiteComparison{TaxonomyVersion: TaxonomyVersion, Disclaimer: CompareDisclaimer,
		Sites: []ComparedSite{}, Capabilities: []CapabilityRow{}, Shared: []string{}, Unique: map[string][]string{},
		Unmapped: map[string][]string{}, Journeys: map[string][]string{}, Technology: []TechnologyRow{}, Pairs: []SitePair{}, Notes: []string{}}

	observed := map[string]map[string]CapabilityCell{} // site -> taxonomy key -> cell
	tech := map[string]map[string]string{}             // tech name -> site -> confidence
	domains := map[string]int{}
	for _, s := range in {
		id := s.Site.WebsiteID
		out.Sites = append(out.Sites, s.Site)
		domains[s.Site.Domain]++
		observed[id] = map[string]CapabilityCell{}
		for _, c := range s.Profile.Capabilities {
			if c.EvidenceClass != "OBSERVED" {
				continue
			}
			if t, ok := taxonomyByName[strings.ToLower(c.Name)]; ok {
				observed[id][t.Key] = CapabilityCell{Present: true, EvidenceClass: c.EvidenceClass, Confidence: c.Confidence, PageCount: len(c.EvidenceRefs)}
			}
		}
		unmapped := []string{}
		for _, f := range s.Findings {
			if f.Kind != KindCapability || f.Source != "AGENT" {
				continue
			}
			if t, ok := taxonomyByName[strings.ToLower(f.Name)]; ok {
				if _, have := observed[id][t.Key]; !have {
					observed[id][t.Key] = CapabilityCell{Present: true, EvidenceClass: f.EvidenceClass, Confidence: f.Confidence, PageCount: len(f.ObservationIDs)}
				}
				continue
			}
			unmapped = append(unmapped, f.Name)
		}
		sort.Strings(unmapped)
		out.Unmapped[id] = dedupeStrings(unmapped)
		journeys := append([]string{}, s.Profile.Journeys...)
		for _, f := range s.Findings {
			if f.Kind == KindJourney && f.Source == "AGENT" {
				journeys = append(journeys, f.Name)
			}
		}
		out.Journeys[id] = dedupeStrings(journeys)
		for _, t := range s.Profile.TechSignals {
			if tech[t.Name] == nil {
				tech[t.Name] = map[string]string{}
			}
			tech[t.Name][id] = t.Confidence
		}
		if s.Site.PageCount <= 1 {
			out.Notes = append(out.Notes, fmt.Sprintf("%s was investigated from a single page; capabilities on other pages are not counted.", s.Site.Domain))
		}
	}
	for d, n := range domains {
		if n > 1 {
			out.Notes = append(out.Notes, fmt.Sprintf("%s appears %d times: these are separate investigations of the same site (e.g. different pages or dates).", d, n))
		}
	}

	for _, t := range CapabilityTaxonomy {
		row := CapabilityRow{Key: t.Key, Name: t.Name, ProposedComponent: t.Service, Sites: map[string]CapabilityCell{}}
		for _, s := range in {
			cell, ok := observed[s.Site.WebsiteID][t.Key]
			if !ok {
				cell = CapabilityCell{Present: false}
			}
			row.Sites[s.Site.WebsiteID] = cell
			if cell.Present {
				row.PresentOn++
			}
		}
		out.Capabilities = append(out.Capabilities, row)
		switch {
		case row.PresentOn == len(in):
			out.Shared = append(out.Shared, t.Name)
		case row.PresentOn == 1:
			for id, c := range row.Sites {
				if c.Present {
					out.Unique[id] = append(out.Unique[id], t.Name)
				}
			}
		}
	}
	for _, s := range in {
		if out.Unique[s.Site.WebsiteID] == nil {
			out.Unique[s.Site.WebsiteID] = []string{}
		}
	}

	names := make([]string, 0, len(tech))
	for n := range tech {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		out.Technology = append(out.Technology, TechnologyRow{Name: n, Sites: tech[n]})
	}

	for i := 0; i < len(in); i++ {
		for j := i + 1; j < len(in); j++ {
			a, b := in[i].Site.WebsiteID, in[j].Site.WebsiteID
			p := SitePair{A: a, B: b, SharedCapabilities: []string{}, SharedTechnology: []string{}}
			union := 0
			for _, t := range CapabilityTaxonomy {
				_, inA := observed[a][t.Key]
				_, inB := observed[b][t.Key]
				if inA || inB {
					union++
				}
				if inA && inB {
					p.SharedCapabilities = append(p.SharedCapabilities, t.Name)
				}
			}
			if union > 0 {
				p.CapabilitySimilarity = math.Round(float64(len(p.SharedCapabilities))/float64(union)*1000) / 1000
			}
			for _, n := range names {
				if _, okA := tech[n][a]; okA {
					if _, okB := tech[n][b]; okB {
						p.SharedTechnology = append(p.SharedTechnology, n)
					}
				}
			}
			out.Pairs = append(out.Pairs, p)
		}
	}
	return out
}
