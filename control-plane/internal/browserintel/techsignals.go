package browserintel

import (
	"fmt"
	"sort"
	"strings"
)

// Technology signals (4.4) are passive only: they are inferred from what the page already
// loaded and the user previewed (resource-timing URLs, public metadata, same-site routes).
// Nothing here — and nothing in this package — sends a request to the investigated site.

type techSignature struct {
	name, category string
	// clue patterns, matched case-insensitively against one source
	resource, generator, route []string
	confidence                 string
}

var techSignatures = []techSignature{
	{name: "Next.js (React)", category: "frontend framework", resource: []string{"/_next/static/"}, route: []string{"/_next/"}, confidence: "MEDIUM"},
	{name: "Nuxt (Vue)", category: "frontend framework", resource: []string{"/_nuxt/"}, confidence: "MEDIUM"},
	{name: "Gatsby", category: "frontend framework", generator: []string{"gatsby"}, resource: []string{"/page-data/"}, confidence: "MEDIUM"},
	{name: "WordPress", category: "content management", generator: []string{"wordpress"}, resource: []string{"/wp-content/", "/wp-includes/"}, confidence: "MEDIUM"},
	{name: "Drupal", category: "content management", generator: []string{"drupal"}, confidence: "MEDIUM"},
	{name: "Shopify", category: "commerce platform", resource: []string{"cdn.shopify.com"}, generator: []string{"shopify"}, confidence: "MEDIUM"},
	{name: "Webflow", category: "site builder", generator: []string{"webflow"}, confidence: "MEDIUM"},
	{name: "Wix", category: "site builder", generator: []string{"wix.com"}, resource: []string{"static.wixstatic.com"}, confidence: "MEDIUM"},
	{name: "Stripe", category: "payments", resource: []string{"js.stripe.com"}, confidence: "MEDIUM"},
	{name: "PayPal", category: "payments", resource: []string{"paypal.com/sdk/js", "paypalobjects.com"}, confidence: "MEDIUM"},
	{name: "Amazon CloudFront", category: "CDN", resource: []string{".cloudfront.net"}, confidence: "MEDIUM"},
	{name: "Cloudflare", category: "CDN / edge", resource: []string{"/cdn-cgi/", "cdnjs.cloudflare.com"}, confidence: "LOW"},
	{name: "Fastly", category: "CDN", resource: []string{".fastly.net", ".fastlylb.net"}, confidence: "MEDIUM"},
	{name: "Akamai", category: "CDN", resource: []string{".akamaihd.net", ".akamaized.net"}, confidence: "MEDIUM"},
	{name: "Google Tag Manager / Analytics", category: "analytics", resource: []string{"googletagmanager.com", "google-analytics.com"}, confidence: "MEDIUM"},
	{name: "Segment", category: "analytics", resource: []string{"cdn.segment.com"}, confidence: "MEDIUM"},
	{name: "Hotjar", category: "analytics", resource: []string{"static.hotjar.com"}, confidence: "MEDIUM"},
	{name: "Sentry", category: "error monitoring", resource: []string{"sentry-cdn.com", ".ingest.sentry.io"}, confidence: "MEDIUM"},
	{name: "Intercom", category: "customer messaging", resource: []string{"widget.intercom.io", "js.intercomcdn.com"}, confidence: "MEDIUM"},
	{name: "Algolia", category: "search service", resource: []string{".algolia.net", ".algolianet.com", "cdn.jsdelivr.net/npm/algoliasearch"}, confidence: "MEDIUM"},
	{name: "Auth0", category: "identity provider", resource: []string{".auth0.com", "cdn.auth0.com"}, confidence: "MEDIUM"},
	{name: "Okta", category: "identity provider", resource: []string{".okta.com"}, confidence: "MEDIUM"},
	{name: "Amazon Cognito", category: "identity provider", resource: []string{".amazoncognito.com"}, confidence: "MEDIUM"},
	{name: "Firebase", category: "backend-as-a-service", resource: []string{"firebaseio.com", "gstatic.com/firebasejs"}, confidence: "MEDIUM"},
	{name: "Google reCAPTCHA", category: "bot protection", resource: []string{"google.com/recaptcha", "recaptcha.net"}, confidence: "MEDIUM"},
	{name: "Google Maps", category: "maps", resource: []string{"maps.googleapis.com"}, confidence: "MEDIUM"},
}

// DetectTechSignals infers technologies from passive clues in the observations. Every
// signal is INFERRED (the clue is observed; the technology is a conclusion), cites the
// observations that show the clue, and carries the clue itself.
func DetectTechSignals(obs []Observation) []CapabilityFinding {
	type hit struct {
		refs []string
		clue string
		conf string
		how  string
	}
	hits := map[string]*hit{}
	for _, o := range obs {
		gen := strings.ToLower(o.Observations.PublicMetadata["generator"] + " " + o.Observations.PublicMetadata["application-name"])
		for _, sig := range techSignatures {
			clue, how, conf := "", "", sig.confidence
			for _, p := range sig.generator {
				if strings.Contains(gen, p) {
					clue, how, conf = o.Observations.PublicMetadata["generator"], "the page's public generator metadata", "HIGH"
					break
				}
			}
			if clue == "" {
				for _, r := range o.Observations.ResourceMetadata {
					for _, p := range sig.resource {
						if strings.Contains(strings.ToLower(r.Name), p) {
							clue, how = r.Name, "a resource the page loaded (resource timing)"
							break
						}
					}
					if clue != "" {
						break
					}
				}
			}
			if clue == "" {
				for _, rt := range o.Observations.PublicRoutes {
					for _, p := range sig.route {
						if strings.HasPrefix(strings.ToLower(rt), p) {
							clue, how = rt, "a public route linked from the page"
							break
						}
					}
				}
			}
			if clue == "" {
				continue
			}
			key := sig.name
			if hits[key] == nil {
				hits[key] = &hit{clue: clue, conf: conf, how: fmt.Sprintf("%s (%s)", how, sig.category)}
			}
			if conf == "HIGH" {
				hits[key].conf = "HIGH"
			}
			hits[key].refs = append(hits[key].refs, "observation:"+o.ID)
		}
	}
	names := make([]string, 0, len(hits))
	for n := range hits {
		names = append(names, n)
	}
	sort.Strings(names)
	out := []CapabilityFinding{}
	for _, n := range names {
		h := hits[n]
		out = append(out, CapabilityFinding{Name: n, EvidenceClass: "INFERRED", EvidenceRefs: dedupeStrings(h.refs), Confidence: h.conf, Clue: h.clue,
			Rationale: fmt.Sprintf("Passive signal: %s references %q. Inferred, not verified; no request was sent to the site.", h.how, h.clue)})
	}
	return out
}

// ClueObserved reports whether a technology clue literally appears in what one of the cited
// observations recorded (resources, public metadata, routes, visible text). Agents must cite
// such a clue; a technology claim without an observed clue would be invention or probing.
func ClueObserved(clue string, obs []Observation) bool {
	clue = strings.ToLower(strings.TrimSpace(clue))
	if len(clue) < 4 {
		return false
	}
	for _, o := range obs {
		var parts []string
		for _, r := range o.Observations.ResourceMetadata {
			parts = append(parts, r.Name)
		}
		for _, v := range o.Observations.PublicMetadata {
			parts = append(parts, v)
		}
		parts = append(parts, o.Observations.PublicRoutes...)
		parts = append(parts, o.Observations.VisibleText...)
		parts = append(parts, o.Observations.Headings...)
		parts = append(parts, o.Observations.VisibleFeatures...)
		for _, p := range parts {
			if strings.Contains(strings.ToLower(p), clue) {
				return true
			}
		}
	}
	return false
}
