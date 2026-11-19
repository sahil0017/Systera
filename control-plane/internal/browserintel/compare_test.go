package browserintel

import "testing"

func site(id, domain string, pages int, caps ...string) ComparisonInput {
	var cs []CapabilityFinding
	for _, c := range caps {
		cs = append(cs, CapabilityFinding{Name: c, EvidenceClass: "OBSERVED", Confidence: "HIGH", EvidenceRefs: []string{"observation:x"}})
		cs = append(cs, CapabilityFinding{Name: c + " Service", EvidenceClass: "PROPOSED"})
	}
	return ComparisonInput{Site: ComparedSite{WebsiteID: id, Domain: domain, PageCount: pages}, Profile: WebsiteProfile{ID: id, Domain: domain, Capabilities: cs,
		TechSignals: []CapabilityFinding{{Name: "Stripe", Confidence: "MEDIUM"}}}}
}

func TestBuildComparisonUsesSharedTaxonomy(t *testing.T) {
	a := site("a", "shop-a.com", 3, "Shopping Cart", "Checkout & Payments", "Product / Content Search")
	b := site("b", "shop-b.com", 1, "Shopping Cart", "Plans & Pricing")
	b.Findings = []Finding{{Kind: KindCapability, Source: "AGENT", Name: "Gift cards", EvidenceClass: "OBSERVED"},
		{Kind: KindCapability, Source: "AGENT", Name: "Product / Content Search", EvidenceClass: "OBSERVED", Confidence: "HIGH", ObservationIDs: []string{"o"}}}
	c := BuildComparison([]ComparisonInput{a, b})
	if len(c.Capabilities) != len(CapabilityTaxonomy) || c.TaxonomyVersion != TaxonomyVersion || c.Disclaimer == "" {
		t.Fatalf("every taxonomy entry is a row: %+v", c)
	}
	if len(c.Shared) != 2 { // cart, and search via the agent finding mapped onto the taxonomy
		t.Fatalf("shared: %v", c.Shared)
	}
	if len(c.Unique["a"]) != 1 || c.Unique["a"][0] != "Checkout & Payments" || len(c.Unique["b"]) != 1 || c.Unique["b"][0] != "Plans & Pricing" {
		t.Fatalf("unique: %v", c.Unique)
	}
	if len(c.Unmapped["b"]) != 1 || c.Unmapped["b"][0] != "Gift cards" {
		t.Fatalf("unmapped: %v", c.Unmapped)
	}
	if len(c.Pairs) != 1 || c.Pairs[0].CapabilitySimilarity != 0.5 || len(c.Pairs[0].SharedTechnology) != 1 {
		t.Fatalf("pairs: %+v", c.Pairs)
	}
	if len(c.Notes) != 1 { // shop-b.com came from a single page
		t.Fatalf("notes: %v", c.Notes)
	}
	for _, row := range c.Capabilities {
		if len(row.Sites) != 2 {
			t.Fatalf("row %s lacks a cell per site", row.Key)
		}
	}
}
