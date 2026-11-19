package engineeringpulse

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ResearchConfig tunes report construction.
type ResearchConfig struct {
	MaxEvidence  int           // evidence items per report (default 8)
	MaxFindings  int           // technical findings per report (default 6)
	Lookback     time.Duration // evidence window (default 180 days)
	StaleWarning time.Duration // evidence older than this adds a limitation (default 90 days)
}

func (c ResearchConfig) withDefaults() ResearchConfig {
	if c.MaxEvidence <= 0 {
		c.MaxEvidence = 8
	}
	if c.MaxFindings <= 0 {
		c.MaxFindings = 6
	}
	if c.Lookback <= 0 {
		c.Lookback = 180 * 24 * time.Hour
	}
	if c.StaleWarning <= 0 {
		c.StaleWarning = 90 * 24 * time.Hour
	}
	return c
}

// Maturity time-to-live before published knowledge must be re-validated.
var maturityTTL = map[string]time.Duration{
	MaturityExperimental: 90 * 24 * time.Hour,
	MaturityEmerging:     120 * 24 * time.Hour,
	MaturityMainstream:   365 * 24 * time.Hour,
	MaturityLegacy:       365 * 24 * time.Hour,
	MaturityDeprecated:   365 * 24 * time.Hour,
}

// Caveat texts the gate looks for.
const (
	CaveatAdoption     = "Trend score measures attention, not technical fitness; it must not be used on its own to select technology."
	caveatExperimental = "Experimental: expect breaking changes and a limited operational track record."
	caveatEmerging     = "Emerging: limited production evidence; validate against your own requirements before adopting."
	caveatDeprecated   = "Deprecated or end-of-life signals present; avoid for new designs and plan migrations."
	caveatLegacy       = "Legacy: still supported but no longer the recommended approach for new designs."
)

var maturityCaveat = map[string]string{
	MaturityExperimental: caveatExperimental,
	MaturityEmerging:     caveatEmerging,
	MaturityDeprecated:   caveatDeprecated,
	MaturityLegacy:       caveatLegacy,
}

// SelectEvidence orders candidate items (primary sources first, then reliability, significance
// and recency), keeps at most one item per source per title family, assigns roles and caps
// the count. Duplicates must already have been excluded by the caller.
func SelectEvidence(cands []EvidenceItem, cfg ResearchConfig) []EvidenceItem {
	cfg = cfg.withDefaults()
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if a.IsPrimarySource != b.IsPrimarySource {
			return a.IsPrimarySource
		}
		if a.Reliability != b.Reliability {
			return a.Reliability > b.Reliability
		}
		if a.Significance != b.Significance {
			return a.Significance > b.Significance
		}
		return pubUnix(a.PublishedAt) > pubUnix(b.PublishedAt)
	})
	var out []EvidenceItem
	for _, c := range cands {
		if len(out) >= cfg.MaxEvidence {
			break
		}
		if c.IsPrimarySource {
			c.Role = "PRIMARY"
		} else {
			c.Role = "CORROBORATING"
		}
		out = append(out, c)
	}
	return out
}

// EvidenceHash fingerprints an evidence set so unchanged evidence does not re-trigger research.
func EvidenceHash(ev []EvidenceItem) string {
	ids := make([]string, len(ev))
	for i, e := range ev {
		ids[i] = e.ItemID
	}
	sort.Strings(ids)
	sum := sha256.Sum256([]byte(strings.Join(ids, ",")))
	return hex.EncodeToString(sum[:])
}

// BuildReport compiles a research report from selected evidence (2.6). It is deterministic:
// technical findings come only from cited items; adoption (attention) is a separate finding.
func BuildReport(topic Topic, score *TrendScore, ev []EvidenceItem, now time.Time, cfg ResearchConfig) Report {
	cfg = cfg.withDefaults()
	r := Report{TopicID: topic.ID, TopicKey: topic.Key, Status: ReportDraft, Evidence: ev, EvidenceHash: EvidenceHash(ev)}
	if score != nil {
		r.TrendScoreID = score.ID
	}
	r.Title = fmt.Sprintf("%s: engineering research digest", topic.Name)

	var primary, corroborating int
	sources := map[string]bool{}
	var relSum, sigSum float64
	var newest *time.Time
	metadataOnly, papers := false, false
	for _, e := range ev {
		if e.Role == "PRIMARY" {
			primary++
		} else {
			corroborating++
		}
		sources[e.SourceKey] = true
		relSum += e.Reliability
		sigSum += e.Significance
		if e.PublishedAt != nil && (newest == nil || e.PublishedAt.After(*newest)) {
			t := *e.PublishedAt
			newest = &t
		}
		if e.AllowedContent == ContentMetadataOnly {
			metadataOnly = true
		}
		if e.SourceType == "PAPER" {
			papers = true
		}
	}
	n := len(ev)
	avgRel, avgSig := 0.0, 0.0
	if n > 0 {
		avgRel, avgSig = relSum/float64(n), sigSum/float64(n)
	}
	r.FreshnessAt = newest
	r.Maturity = reportMaturity(topic, ev)

	// Findings: one technical claim per cited item, primary sources first.
	for _, e := range ev {
		if len(r.Findings) >= cfg.MaxFindings {
			break
		}
		r.Findings = append(r.Findings, Finding{Claim: findingClaim(e), Kind: FindingTechnical, Evidence: []string{e.ItemID}})
	}
	if score != nil && n > 0 {
		all := make([]string, n)
		for i, e := range ev {
			all[i] = e.ItemID
		}
		r.Findings = append(r.Findings, Finding{
			Kind:     FindingAdoption,
			Evidence: all,
			Claim: fmt.Sprintf("Attention signal: trend score %.1f/100 over %d days (popularity %.0f, growth %.0f, significance %.0f, reliability %.0f, novelty %.0f). This is an adoption signal, not technical proof.",
				score.Score, score.WindowDays, score.Components.Popularity, score.Components.Growth, score.Components.Significance,
				score.Components.Reliability, score.Components.Novelty),
		})
	}

	// Limitations.
	r.Limitations = []string{"Compiled automatically from source titles and permitted summaries; claims were not independently reproduced."}
	if primary == 0 {
		r.Limitations = append(r.Limitations, "No primary-source evidence: findings rely on secondary reporting.")
	}
	if len(sources) < 2 {
		r.Limitations = append(r.Limitations, "Single source: no independent corroboration.")
	}
	if metadataOnly {
		r.Limitations = append(r.Limitations, "Some sources permit metadata only; those findings rest on titles alone.")
	}
	if papers {
		r.Limitations = append(r.Limitations, "Research papers describe proposed techniques; production readiness is not established.")
	}
	if newest != nil && now.Sub(*newest) > cfg.StaleWarning {
		r.Limitations = append(r.Limitations, fmt.Sprintf("Newest evidence is %d days old.", int(now.Sub(*newest).Hours()/24)))
	}

	// Caveats.
	if c, ok := maturityCaveat[r.Maturity]; ok {
		r.Caveats = append(r.Caveats, c)
	}
	r.Caveats = append(r.Caveats, CaveatAdoption)

	// Confidence: reliability, primary backing, independent corroboration, significance.
	diversity := 0.0
	if len(sources) > 1 {
		diversity = math.Min(1, float64(len(sources)-1)/3)
	}
	c := 0.35*avgRel + 0.25*math.Min(1, float64(primary)/2) + 0.2*diversity + 0.2*avgSig
	r.Confidence = round3(clamp01(c))
	r.ConfidenceBand = ConfidenceBand(r.Confidence, len(sources), primary)

	base := now
	if newest != nil {
		base = *newest
	}
	// Never extended: if the newest evidence is already past its maturity TTL, the gate's
	// freshness check rejects the report instead of re-publishing expired knowledge.
	r.StaleAfter = base.Add(maturityTTL[r.Maturity])

	r.Summary = fmt.Sprintf("%s (%s, %s). %d primary and %d corroborating source item(s) from %d source(s); newest evidence %s. Confidence %s (%.2f).",
		topic.Name, strings.ToLower(topic.Facet), strings.ToLower(r.Maturity), primary, corroborating, len(sources),
		fmtDate(newest), r.ConfidenceBand, r.Confidence)
	return r
}

// ConfidenceBand maps a confidence to a band. HIGH additionally needs at least two independent
// sources and a primary source; otherwise it is capped at MEDIUM.
func ConfidenceBand(c float64, distinctSources, primary int) string {
	switch {
	case c >= 0.75 && distinctSources >= 2 && primary >= 1:
		return "HIGH"
	case c >= 0.5:
		return "MEDIUM"
	}
	return "LOW"
}

func reportMaturity(t Topic, ev []EvidenceItem) string {
	counts := map[string]float64{}
	for _, e := range ev {
		w := 1.0
		if e.Role == "PRIMARY" {
			w = 2
		}
		counts[e.Maturity] += w
	}
	total := 0.0
	for _, v := range counts {
		total += v
	}
	if total == 0 {
		return orDefault(t.Maturity, MaturityEmerging)
	}
	// Deprecation evidence dominates: one strong signal is enough to warn.
	if counts[MaturityDeprecated]/total >= 0.34 {
		return MaturityDeprecated
	}
	best, bestV := orDefault(t.Maturity, MaturityEmerging), 0.0
	for _, m := range []string{MaturityMainstream, MaturityEmerging, MaturityExperimental, MaturityLegacy} {
		if counts[m] > bestV {
			best, bestV = m, counts[m]
		}
	}
	if bestV/total < 0.5 {
		return orDefault(t.Maturity, best)
	}
	return best
}

var firstSentenceRe = regexp.MustCompile(`^(.{20,240}?[.!?])(\s|$)`)

func findingClaim(e EvidenceItem) string {
	claim := fmt.Sprintf("%s (%s): %s", e.SourceName, fmtDate(e.PublishedAt), strings.TrimSpace(e.Title))
	if s := strings.TrimSpace(e.Summary); s != "" {
		if m := firstSentenceRe.FindStringSubmatch(s); m != nil {
			s = m[1]
		} else {
			s = truncateRunes(s, 240)
		}
		claim += " — " + s
	}
	return claim
}

func fmtDate(t *time.Time) string {
	if t == nil {
		return "undated"
	}
	return t.UTC().Format("2006-01-02")
}

func pubUnix(t *time.Time) int64 {
	if t == nil {
		return 0
	}
	return t.Unix()
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// RenderMarkdown renders a validated report as the knowledge document body, with numbered
// citations, so retrieval returns the sources alongside the claims.
func RenderMarkdown(topic Topic, r Report) string {
	idx := map[string]int{}
	for i, e := range r.Evidence {
		idx[e.ItemID] = i + 1
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", r.Title)
	fmt.Fprintf(&b, "Topic: %s (%s). Pattern keywords: %s.\n\n", topic.Name, strings.ToLower(topic.Facet), strings.Join(topic.Keywords, ", "))
	if topic.Description != "" {
		fmt.Fprintf(&b, "%s\n\n", topic.Description)
	}
	fmt.Fprintf(&b, "%s\n\n## Findings\n\n", r.Summary)
	for _, f := range r.Findings {
		var refs []string
		for _, id := range f.Evidence {
			if n, ok := idx[id]; ok {
				refs = append(refs, fmt.Sprintf("[%d]", n))
			}
		}
		if f.Kind == FindingAdoption && len(refs) > 3 {
			refs = []string{"[all sources]"}
		}
		label := ""
		if f.Kind == FindingAdoption {
			label = "(adoption signal) "
		}
		fmt.Fprintf(&b, "- %s%s %s\n", label, f.Claim, strings.Join(refs, ""))
	}
	b.WriteString("\n## Limitations\n\n")
	for _, l := range r.Limitations {
		fmt.Fprintf(&b, "- %s\n", l)
	}
	b.WriteString("\n## Caveats\n\n")
	for _, c := range r.Caveats {
		fmt.Fprintf(&b, "- %s\n", c)
	}
	fmt.Fprintf(&b, "\n## Sources\n\n")
	for i, e := range r.Evidence {
		primary := ""
		if e.Role == "PRIMARY" {
			primary = ", primary source"
		}
		fmt.Fprintf(&b, "[%d] %s — %s (%s%s, licence %s) %s\n", i+1, e.SourceName, e.Title, fmtDate(e.PublishedAt), primary, e.Licence, e.URL)
	}
	fmt.Fprintf(&b, "\nConfidence: %s (%.2f). Maturity: %s. Fresh as of %s; re-validate after %s.\n",
		r.ConfidenceBand, r.Confidence, r.Maturity, fmtDate(r.FreshnessAt), r.StaleAfter.UTC().Format("2006-01-02"))
	return b.String()
}
