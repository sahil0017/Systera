// Package browserintel implements the Engineering Lens backend (spec §2.3, §7, §20.3):
// consent-recorded browser sessions, server-side re-sanitization of untrusted page
// observations, URL / domain / selected-page policy, bounded multi-page investigations,
// cascading deletion and retention, website profiles and "Continue in workspace".
package browserintel

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/systera/control-plane/internal/knowledge"
)

const (
	MaxObservationBytes = 256 * 1024 // per observation (request body and stored JSON)
	MaxItemsPerSection  = 200
	MaxTextLen          = 500
	MaxPagesPerSession  = 25
	MaxRetentionDays    = 90
	DefaultRetention    = 30
)

// ConsentVersion identifies the consent text the extension shows (ConsentText). A session
// can only be opened against the current version.
const ConsentVersion = "lens-consent-v1"

// ConsentText is what the user agrees to before anything is sent.
const ConsentText = "Systera will receive only what you preview: the page URL (without credentials or secret query parameters), " +
	"title, visible headings, navigation labels, button and form labels (never what is typed), short visible text, public page " +
	"metadata and limited resource timing (URLs without query strings). Passwords, cookies, tokens, payment data, personal values, " +
	"request bodies, history and hidden content are never collected. You can remove items before sending, cancel, or delete the " +
	"session and everything derived from it at any time; it is deleted automatically when retention expires."

var (
	ErrNotFound            = errors.New("browser resource not found")
	ErrInvalidInput        = errors.New("invalid browser observation input")
	ErrURLPolicyDenied     = errors.New("URL rejected by browser collection policy")
	ErrPayloadTooLarge     = errors.New("observation payload exceeds maximum allowed size")
	ErrPageLimitReached    = errors.New("session maximum page count reached")
	ErrConsentRequired     = errors.New("explicit user consent is required")
	ErrSessionClosed       = errors.New("session no longer accepts observations")
	ErrDuplicatePage       = errors.New("page already captured in this session; delete it first to recapture")
	ErrPromotionInProgress = errors.New("promotion already in progress")
	ErrNoObservations      = errors.New("session has no observations to analyze")
)

// AnalysisMode enumerates Engineering Lens collection/analysis modes (§7.6).
type AnalysisMode string

const (
	ModeQuickScan        AnalysisMode = "quick_scan"
	ModeProductMap       AnalysisMode = "product_map"
	ModeFullSystemDesign AnalysisMode = "full_system_design"
	ModeCompareWebsites  AnalysisMode = "compare_websites"
)

func (m AnalysisMode) valid() bool {
	switch m {
	case ModeQuickScan, ModeProductMap, ModeFullSystemDesign, ModeCompareWebsites:
		return true
	}
	return false
}

// Session tracks a user-initiated, consent-recorded browser investigation.
type Session struct {
	ID                 string       `json:"id"`
	TenantID           string       `json:"tenant_id"`
	UserID             string       `json:"user_id"`
	Domain             string       `json:"domain"`
	AnalysisMode       AnalysisMode `json:"analysis_mode"`
	AllowedPages       []string     `json:"allowed_pages"`
	MaxPages           int          `json:"max_pages"`
	ConsentConfirmed   bool         `json:"consent_confirmed"`
	ConsentVersion     string       `json:"consent_version"`
	ConsentedAt        time.Time    `json:"consented_at"`
	RetentionExpiresAt time.Time    `json:"retention_expires_at"`
	Status             string       `json:"status"`
	ObservationCount   int          `json:"observation_count"`
	CreatedAt          time.Time    `json:"created_at"`
	UpdatedAt          time.Time    `json:"updated_at"`
}

// CreateSessionInput opens a new browser investigation session. AllowedPages (multi-page
// mode) bounds the investigation to user-selected pages on Domain.
type CreateSessionInput struct {
	Domain           string       `json:"domain"`
	AnalysisMode     AnalysisMode `json:"analysis_mode"`
	AllowedPages     []string     `json:"allowed_pages,omitempty"`
	MaxPages         int          `json:"max_pages,omitempty"`
	ConsentConfirmed bool         `json:"consent_confirmed"`
	ConsentVersion   string       `json:"consent_version"`
	RetentionDays    int          `json:"retention_days,omitempty"`
}

// FormObservation captures only form labels/action path—never entered user values (§7.3).
type FormObservation struct {
	Action string   `json:"action,omitempty"`
	Method string   `json:"method,omitempty"`
	Labels []string `json:"labels"`
}

// ResourceMetadata captures passive, publicly observable resource timing (§8.3).
type ResourceMetadata struct {
	Name          string  `json:"name"`
	InitiatorType string  `json:"initiator_type,omitempty"`
	DurationMs    float64 `json:"duration_ms,omitempty"`
}

// ObservationPayload is the normalized page observation document (§7.4).
type ObservationPayload struct {
	Headings         []string           `json:"headings"`
	Navigation       []string           `json:"navigation"`
	VisibleFeatures  []string           `json:"visible_features"`
	VisibleText      []string           `json:"visible_text"`
	Forms            []FormObservation  `json:"forms"`
	PublicRoutes     []string           `json:"public_routes"`
	PublicMetadata   map[string]string  `json:"public_metadata"`
	ResourceMetadata []ResourceMetadata `json:"resource_metadata"`
}

// ObservationInput submits a bounded page snapshot to an active session.
type ObservationInput struct {
	SessionID        string             `json:"session_id"`
	SchemaVersion    string             `json:"schema_version,omitempty"`
	CollectorVersion string             `json:"collector_version,omitempty"`
	URL              string             `json:"url"`
	Title            string             `json:"title"`
	UserInitiated    bool               `json:"user_initiated"`
	Observations     ObservationPayload `json:"observations"`
	Redactions       []string           `json:"redactions_applied,omitempty"`
}

// Observation is a server-re-sanitized, untrusted page snapshot.
type Observation struct {
	ID                 string             `json:"id"`
	TenantID           string             `json:"tenant_id"`
	SessionID          string             `json:"session_id"`
	URL                string             `json:"url"`
	Title              string             `json:"title"`
	CollectorVersion   string             `json:"collector_version"`
	SchemaVersion      string             `json:"schema_version"`
	Observations       ObservationPayload `json:"observations"`
	RedactionsApplied  []string           `json:"redactions_applied"`
	InjectionSuspected bool               `json:"injection_suspected"`
	Untrusted          bool               `json:"untrusted"`
	PayloadBytes       int                `json:"payload_bytes"`
	CapturedAt         time.Time          `json:"captured_at"`
}

// CapabilityFinding classifies a detected product capability with explicit evidence class (§8.2).
type CapabilityFinding struct {
	Name          string   `json:"name"`
	EvidenceClass string   `json:"evidence_class"` // OBSERVED | INFERRED | PROPOSED
	EvidenceRefs  []string `json:"evidence_refs"`
	Confidence    string   `json:"confidence"`
	Rationale     string   `json:"rationale"`
	Clue          string   `json:"clue,omitempty"` // technology signals: the passive clue observed
}

// ArchitectureAlternative represents a counterfactual architecture option (§2.6, §8.4).
type ArchitectureAlternative struct {
	Name      string   `json:"name"`
	Summary   string   `json:"summary"`
	Strengths []string `json:"strengths"`
	Risks     []string `json:"risks"`
	BestWhen  string   `json:"best_when"`
}

// WebsiteProfile holds what was derived from a session's observations.
type WebsiteProfile struct {
	ID                   string                    `json:"id"`
	TenantID             string                    `json:"tenant_id"`
	SessionID            string                    `json:"session_id"`
	Domain               string                    `json:"domain"`
	ProjectID            string                    `json:"project_id,omitempty"`
	ModelID              string                    `json:"model_id,omitempty"`
	RunID                string                    `json:"run_id,omitempty"`
	Capabilities         []CapabilityFinding       `json:"capabilities"`
	Journeys             []string                  `json:"journeys"`
	TechSignals          []CapabilityFinding       `json:"tech_signals"`
	Fingerprint          map[string]any            `json:"fingerprint"`
	Alternatives         []ArchitectureAlternative `json:"alternatives"`
	SourceObservationIDs []string                  `json:"source_observation_ids"`
	Status               string                    `json:"status"`
	PromotedAt           *time.Time                `json:"promoted_at,omitempty"`
	CreatedAt            time.Time                 `json:"created_at"`
	UpdatedAt            time.Time                 `json:"updated_at"`
}

// AnalyzeInput triggers website intelligence derivation for a session.
type AnalyzeInput struct {
	SessionID          string   `json:"session_id"`
	RequestedArtifacts []string `json:"requested_artifacts,omitempty"`
}

// DeletionReport says what a deletion removed (shown to the user).
type DeletionReport struct {
	SessionID    string `json:"session_id,omitempty"`
	Observations int    `json:"observations_deleted"`
	Profiles     int    `json:"derived_profiles_deleted"`
	// LedgerEvidenceRevoked counts architecture-model evidence revoked (its claims turn STALE).
	LedgerEvidenceRevoked int    `json:"ledger_evidence_revoked"`
	Status                string `json:"status"`
}

var (
	sensitiveQueryKeys = map[string]bool{
		"token": true, "access_token": true, "id_token": true, "refresh_token": true, "auth": true, "code": true,
		"key": true, "api_key": true, "apikey": true, "secret": true, "client_secret": true, "signature": true, "sig": true,
		"password": true, "pwd": true, "pass": true, "session": true, "sessionid": true, "sid": true, "cookie": true,
		"jwt": true, "email": true, "phone": true, "otp": true, "state": true, "nonce": true,
	}
	secretPatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)bearer\s+[a-z0-9._\-~+/]{12,}=*`),
		regexp.MustCompile(`eyJ[a-zA-Z0-9_\-]{10,}\.[a-zA-Z0-9_\-]{10,}\.[a-zA-Z0-9_\-]{10,}`), // JWT
		regexp.MustCompile(`\b(?:sk|pk|rk|gsk|ghp|gho|xox[abpr]|AKIA|AIza)[-_]?[a-zA-Z0-9_\-]{12,}\b`),
		regexp.MustCompile(`(?i)\b(?:password|passwd|pwd|cookie|set-cookie|secret|api[_-]?key|token|session(?:id)?)\s*[:=]\s*\S+`),
		regexp.MustCompile(`\b(?:\d[ -]?){13,19}\b`),                                // card / account-number-like digit runs
		regexp.MustCompile(`\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}\b`),  // email addresses
		regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`),                                 // US SSN shape
		regexp.MustCompile(`(?:\+\d{1,3}[ .-]?)?\(?\d{3}\)?[ .-]\d{3}[ .-]\d{4}\b`), // phone numbers
		regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
	}
	redactionLabelRe = regexp.MustCompile(`^[a-z][a-z0-9_]{1,63}$`)
	hostnameRe       = regexp.MustCompile(`^(?i)[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+$`)

	// Public page metadata that may be kept (anything else is dropped).
	allowedMetadata = map[string]bool{
		"description": true, "keywords": true, "generator": true, "application-name": true, "robots": true,
		"og:title": true, "og:type": true, "og:site_name": true, "og:description": true, "twitter:card": true,
		"lang": true, "canonical": true, "manifest": true,
	}
	urlMetadata = map[string]bool{"canonical": true, "manifest": true}
)

func isBlockedHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") ||
		strings.HasSuffix(host, ".internal") || strings.HasSuffix(host, ".lan") || strings.HasSuffix(host, ".home.arpa") {
		return true
	}
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
			ip.IsUnspecified() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast()
	}
	return false
}

// ValidateAndCleanURL enforces URL policy (public http/https only; no localhost, private,
// link-local or metadata addresses) and strips credentials, fragments and sensitive query
// parameters (§7.3).
func ValidateAndCleanURL(rawURL string) (string, bool, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", false, fmt.Errorf("%w: only public http/https URLs are permitted", ErrURLPolicyDenied)
	}
	if isBlockedHost(u.Hostname()) {
		return "", false, fmt.Errorf("%w: local, private or internal host %q is blocked", ErrURLPolicyDenied, u.Hostname())
	}
	redacted := false
	if u.User != nil {
		u.User = nil
		redacted = true
	}
	if u.Fragment != "" || u.RawFragment != "" {
		u.Fragment, u.RawFragment = "", ""
		redacted = true
	}
	q := u.Query()
	for k := range q {
		if sensitiveQueryKeys[strings.ToLower(k)] {
			q.Del(k)
			redacted = true
		}
	}
	u.RawQuery = q.Encode()
	u.Host = strings.ToLower(u.Host)
	return u.String(), redacted, nil
}

// ValidateDomain normalizes and checks a session domain (a public hostname).
func ValidateDomain(d string) (string, error) {
	d = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(d), "."))
	if !hostnameRe.MatchString(d) || isBlockedHost(d) {
		return "", fmt.Errorf("%w: domain must be a public hostname", ErrURLPolicyDenied)
	}
	return d, nil
}

// NormalizePage reduces a URL to scheme://host/path, the identity used for selected pages.
func NormalizePage(raw string) (string, error) {
	clean, _, err := ValidateAndCleanURL(raw)
	if err != nil {
		return "", err
	}
	u, _ := url.Parse(clean)
	p := strings.TrimRight(u.EscapedPath(), "/")
	if p == "" {
		p = "/"
	}
	return u.Scheme + "://" + u.Host + p, nil
}

// OnDomain reports whether host is the session domain or one of its subdomains
// ("www.example.com" and "shop.example.com" are on "example.com").
func OnDomain(host, domain string) bool {
	host = strings.ToLower(host)
	domain = strings.TrimPrefix(strings.ToLower(domain), "www.")
	return host == domain || strings.HasSuffix(host, "."+domain)
}

// CheckPage enforces that an observation belongs to the session: same domain and, in
// multi-page mode, one of the user-selected pages.
func CheckPage(s Session, cleanURL string) error {
	u, err := url.Parse(cleanURL)
	if err != nil {
		return fmt.Errorf("%w: unparsable URL", ErrURLPolicyDenied)
	}
	if !OnDomain(u.Hostname(), s.Domain) {
		return fmt.Errorf("%w: %s is not on the session domain %s", ErrURLPolicyDenied, u.Hostname(), s.Domain)
	}
	if len(s.AllowedPages) == 0 {
		return nil
	}
	page, err := NormalizePage(cleanURL)
	if err != nil {
		return err
	}
	for _, a := range s.AllowedPages {
		if a == page {
			return nil
		}
	}
	return fmt.Errorf("%w: %s is not one of the pages selected for this investigation", ErrURLPolicyDenied, page)
}

// truncate cuts to at most n bytes without splitting a UTF-8 sequence.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// pathOnly keeps only the path of a URL or relative reference (no host, query or fragment).
func pathOnly(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if u.Scheme != "" && u.Scheme != "http" && u.Scheme != "https" {
		return "" // javascript:, data:, mailto: ...
	}
	p := u.EscapedPath()
	if p == "" && u.Host == "" {
		return ""
	}
	if p == "" {
		p = "/"
	}
	return p
}

// resourceName keeps origin + path of a resource URL; queries routinely carry tokens.
func resourceName(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || isBlockedHost(u.Hostname()) {
		return ""
	}
	return u.Scheme + "://" + strings.ToLower(u.Host) + u.EscapedPath()
}

// SanitizeObservation re-applies every §7.3 exclusion server-side, so a modified extension
// cannot widen collection. It returns the redactions applied and whether the page text looks
// like a prompt-injection attempt (observations are untrusted data either way).
func SanitizeObservation(in *ObservationInput) (redactions []string, injectionSuspected bool, err error) {
	if !in.UserInitiated {
		return nil, false, ErrConsentRequired
	}
	if in.SchemaVersion == "" {
		in.SchemaVersion = "1.0"
	}
	if in.SchemaVersion != "1.0" {
		return nil, false, fmt.Errorf("%w: unsupported schema_version %q", ErrInvalidInput, in.SchemaVersion)
	}
	if in.CollectorVersion == "" {
		in.CollectorVersion = "1.0"
	}
	if len(in.CollectorVersion) > 32 {
		return nil, false, fmt.Errorf("%w: collector_version too long", ErrInvalidInput)
	}
	cleanURL, urlRedacted, err := ValidateAndCleanURL(in.URL)
	if err != nil {
		return nil, false, err
	}
	in.URL = cleanURL
	if urlRedacted {
		redactions = append(redactions, "url_sensitive_parts_stripped")
	}

	redactStr := func(s string) string {
		s = strings.ToValidUTF8(s, "")
		s = strings.Join(strings.Fields(s), " ")
		if knowledge.DetectPromptInjection(s) {
			injectionSuspected = true
		}
		for _, re := range secretPatterns {
			if re.MatchString(s) {
				s = re.ReplaceAllString(s, "[REDACTED]")
				redactions = append(redactions, "secret_or_pii_redacted")
			}
		}
		return strings.TrimSpace(truncate(s, MaxTextLen))
	}
	cap := func(n int, section string) int {
		if n > MaxItemsPerSection {
			redactions = append(redactions, "section_truncated_"+section)
			return MaxItemsPerSection
		}
		return n
	}
	texts := func(xs []string, section string) []string {
		xs = xs[:cap(len(xs), section)]
		out := make([]string, 0, len(xs))
		seen := map[string]bool{}
		for _, x := range xs {
			if c := redactStr(x); c != "" && !seen[c] {
				seen[c] = true
				out = append(out, c)
			}
		}
		return out
	}
	o := &in.Observations
	in.Title = redactStr(in.Title)
	o.Headings = texts(o.Headings, "headings")
	o.Navigation = texts(o.Navigation, "navigation")
	o.VisibleFeatures = texts(o.VisibleFeatures, "visible_features")
	o.VisibleText = texts(o.VisibleText, "visible_text")

	routes := make([]string, 0, len(o.PublicRoutes))
	for _, r := range o.PublicRoutes[:cap(len(o.PublicRoutes), "public_routes")] {
		r = strings.TrimSpace(r)
		if !strings.HasPrefix(r, "/") || strings.HasPrefix(r, "//") || strings.HasPrefix(r, "/\\") {
			continue // routes are same-site paths; protocol-relative references point elsewhere
		}
		if p := pathOnly(r); p != "" {
			if p != r {
				redactions = append(redactions, "route_query_stripped")
			}
			routes = append(routes, redactStr(p))
		}
	}
	o.PublicRoutes = dedupeStrings(routes)

	forms := make([]FormObservation, 0, len(o.Forms))
	for _, f := range o.Forms[:cap(len(o.Forms), "forms")] {
		method := strings.ToUpper(strings.TrimSpace(f.Method))
		if method != "POST" && method != "GET" && method != "DIALOG" {
			method = ""
		}
		forms = append(forms, FormObservation{Action: redactStr(pathOnly(f.Action)), Method: method, Labels: texts(f.Labels, "form_labels")})
	}
	o.Forms = forms

	meta := map[string]string{}
	for k, v := range o.PublicMetadata {
		k = strings.ToLower(strings.TrimSpace(k))
		if !allowedMetadata[k] {
			redactions = append(redactions, "non_public_metadata_dropped")
			continue
		}
		if urlMetadata[k] {
			if c, _, err := ValidateAndCleanURL(v); err == nil {
				meta[k] = c
			}
			continue
		}
		if c := redactStr(v); c != "" {
			meta[k] = c
		}
	}
	o.PublicMetadata = meta

	res := make([]ResourceMetadata, 0, len(o.ResourceMetadata))
	for _, rm := range o.ResourceMetadata[:cap(len(o.ResourceMetadata), "resource_metadata")] {
		name := resourceName(rm.Name)
		if name == "" {
			continue
		}
		if name != rm.Name {
			redactions = append(redactions, "resource_query_stripped")
		}
		d := rm.DurationMs
		if d < 0 || d > 600000 {
			d = 0
		}
		res = append(res, ResourceMetadata{Name: name, InitiatorType: truncate(strings.ToLower(strings.TrimSpace(rm.InitiatorType)), 32), DurationMs: d})
	}
	o.ResourceMetadata = res

	// Client-reported redaction labels are informational; keep only well-formed ones.
	for i, r := range in.Redactions {
		if i >= 32 {
			break
		}
		if redactionLabelRe.MatchString(r) {
			redactions = append(redactions, r)
		}
	}
	redactions = dedupeStrings(redactions)
	sort.Strings(redactions)
	return redactions, injectionSuspected, nil
}

// FrameForAgent renders an observation for model context strictly as data: page text is
// wrapped in an <untrusted_observation> element and can never act as instructions (§7.3).
func FrameForAgent(o Observation) string {
	var b strings.Builder
	flag := ""
	if o.InjectionSuspected {
		flag = ` warning="prompt_injection_pattern_detected"`
	}
	fmt.Fprintf(&b, "<untrusted_observation source=%q url=%q%s>\n", "observation:"+o.ID, o.URL, flag)
	esc := func(s string) string { return strings.NewReplacer("<", "‹", ">", "›").Replace(s) }
	line := func(label string, xs []string) {
		if len(xs) > 0 {
			q := make([]string, len(xs))
			for i, x := range xs {
				q[i] = esc(x)
			}
			fmt.Fprintf(&b, "%s: %s\n", label, strings.Join(q, " | "))
		}
	}
	fmt.Fprintf(&b, "title: %s\n", esc(o.Title))
	line("headings", o.Observations.Headings)
	line("navigation", o.Observations.Navigation)
	line("features", o.Observations.VisibleFeatures)
	line("text", o.Observations.VisibleText)
	line("routes", o.Observations.PublicRoutes)
	for _, f := range o.Observations.Forms {
		line("form "+f.Method+" "+esc(f.Action), f.Labels)
	}
	b.WriteString("</untrusted_observation>")
	return b.String()
}

// TaxonomyEntry is one capability of the shared capability taxonomy (§7.6): every
// investigation is classified against the same entries, so investigations of different
// websites are directly comparable.
type TaxonomyEntry struct {
	Key      string   `json:"key"`
	Name     string   `json:"name"`
	Service  string   `json:"proposed_component"`
	Keywords []string `json:"-"`
}

// TaxonomyVersion identifies the capability taxonomy a profile was classified with.
const TaxonomyVersion = "capability-taxonomy/1"

// CapabilityTaxonomy is the shared capability taxonomy.
var CapabilityTaxonomy = []TaxonomyEntry{
	{"search", "Product / Content Search", "Search & Indexing Service", []string{"search"}},
	{"cart", "Shopping Cart", "Cart Service", []string{"cart", "basket"}},
	{"checkout", "Checkout & Payments", "Order & Payment Orchestrator", []string{"checkout", "payment"}},
	{"auth", "User Authentication", "Identity & Session Service", []string{"login", "log in", "sign in", "sign up", "register"}},
	{"catalogue", "Catalogue Browsing", "Catalogue Service", []string{"catalog", "catalogue", "products", "shop"}},
	{"notifications", "Subscriptions & Notifications", "Notification Service", []string{"subscribe", "newsletter"}},
	{"upload", "Content Upload", "Media Storage & Processing", []string{"upload"}},
	{"pricing", "Plans & Pricing", "Billing Service", []string{"pricing", "plans"}},
}

// DeriveWebsiteProfile builds an evidence-labelled profile from observations. Observed
// capabilities cite the observations whose visible text mentions them; the services that
// would support them are PROPOSED (a hypothesis, not a map of hidden infrastructure).
func DeriveWebsiteProfile(domain string, obs []Observation) WebsiteProfile {
	pageText := map[string]string{}
	var ids []string
	for _, o := range obs {
		var sb strings.Builder
		sb.WriteString(o.Title)
		for _, xs := range [][]string{o.Observations.Headings, o.Observations.Navigation, o.Observations.VisibleFeatures, o.Observations.VisibleText} {
			for _, x := range xs {
				sb.WriteString(" " + x)
			}
		}
		for _, f := range o.Observations.Forms {
			sb.WriteString(" " + strings.Join(f.Labels, " "))
		}
		pageText[o.ID] = " " + strings.ToLower(sb.String()) + " "
		ids = append(ids, o.ID)
	}
	sort.Strings(ids)

	var caps []CapabilityFinding
	found := map[string]bool{}
	for _, c := range CapabilityTaxonomy {
		var refs []string
		var hit string
		for _, id := range ids {
			for _, kw := range c.Keywords {
				if regexp.MustCompile(`[^a-z0-9]` + regexp.QuoteMeta(kw) + `[^a-z0-9]`).MatchString(pageText[id]) {
					refs = append(refs, "observation:"+id)
					if hit == "" {
						hit = kw
					}
					break
				}
			}
		}
		if len(refs) == 0 {
			continue
		}
		found[c.Name] = true
		caps = append(caps,
			CapabilityFinding{Name: c.Name, EvidenceClass: "OBSERVED", EvidenceRefs: refs, Confidence: "HIGH",
				Rationale: fmt.Sprintf("Visible page structure on %d page(s) references %q.", len(refs), hit)},
			CapabilityFinding{Name: c.Service, EvidenceClass: "PROPOSED", EvidenceRefs: refs, Confidence: "MEDIUM",
				Rationale: fmt.Sprintf("Proposed logical component supporting the observed %s capability; not observed directly.", c.Name)})
	}

	var journeys []string
	if found["Shopping Cart"] || found["Checkout & Payments"] {
		journeys = append(journeys, "Browse -> Product Detail -> Cart -> Checkout -> Confirmation")
	}
	if found["Product / Content Search"] {
		journeys = append(journeys, "Query -> Filtered Results -> Item Inspection")
	}
	if found["User Authentication"] {
		journeys = append(journeys, "Landing -> Sign in / Sign up -> Account")
	}
	if len(journeys) == 0 {
		journeys = append(journeys, "Landing -> Navigation -> Primary Action")
	}

	alternatives := []ArchitectureAlternative{
		{Name: "Modular Monolith", Summary: "Single deployable with strict internal domain modules and shared PostgreSQL.",
			Strengths: []string{"Fast delivery", "Simple ACID transactions", "Low operational overhead"},
			Risks:     []string{"Requires module boundary discipline", "Coarse-grained scaling"},
			BestWhen:  "Small-to-medium team, early product iteration, moderate traffic."},
		{Name: "Domain-Oriented Microservices", Summary: "Independent services per bounded context over versioned APIs and an event broker.",
			Strengths: []string{"Independent scaling and deployment", "Strong team ownership boundaries"},
			Risks:     []string{"Distributed transactions", "Higher observability and operational complexity"},
			BestWhen:  "Large engineering org, uneven traffic hotspots, independent release cadences."},
	}
	if caps == nil {
		caps = []CapabilityFinding{}
	}
	return WebsiteProfile{
		Domain: domain, Capabilities: caps, Journeys: journeys, TechSignals: DetectTechSignals(obs), Alternatives: alternatives,
		SourceObservationIDs: ids, Status: "ANALYZED",
		Fingerprint: map[string]any{
			"disclaimer":        "Hypothesis derived from publicly observable UI only; it is not a map of private backend infrastructure.",
			"observation_count": len(obs), "capabilities": caps, "journeys": journeys,
		},
	}
}

func dedupeStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
