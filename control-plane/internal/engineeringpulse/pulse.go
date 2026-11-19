// Package engineeringpulse implements Engineering Pulse (spec §2.2, §6.2, §6.5, §20.2):
//
//	approved source registry -> connectors (respecting terms and rate limits)
//	-> trend items with retrieval metadata -> deduplication to a canonical item
//	-> topic classification (domain / technology / pattern / maturity)
//	-> trend scoring  Score = 0.25P + 0.25G + 0.20E + 0.20R + 0.10N
//	-> research reports (primary-source evidence, limitations, confidence, freshness)
//	-> validation gate -> knowledge_updates (PUBLIC knowledge) -> agent-domain notifications
//	-> retrieval during design runs, with an explanation and supporting sources.
//
// The trend score is an adoption signal only. It never selects technology on its own:
// CompareOptions ranks options by maturity, operational burden, cost, security and fit,
// and uses trend score at most as a tie-break (§6.5).
package engineeringpulse

import (
	"errors"
	"time"
)

var (
	ErrNotFound            = errors.New("not found")
	ErrInvalidInput        = errors.New("invalid input")
	ErrIncompleteCriteria  = errors.New("option assessment is missing engineering criteria")
	ErrSourceNotApproved   = errors.New("source is not approved")
	ErrRateLimited         = errors.New("source rate limit reached")
	ErrDisallowedRedirect  = errors.New("redirect to a different host is not allowed")
	ErrResponseTooLarge    = errors.New("source response exceeds the size limit")
	ErrUnsupportedResponse = errors.New("unsupported source response")
)

// Connector kinds.
const (
	ConnectorGitHubReleases = "GITHUB_RELEASES"
	ConnectorRSS            = "RSS" // RSS 2.0 and Atom
	ConnectorArXiv          = "ARXIV"
)

// Allowed content levels, from a source's terms of use.
const (
	ContentMetadataOnly = "METADATA_ONLY"
	ContentSummary      = "SUMMARY"
	ContentFullText     = "FULL_TEXT"
)

// Source statuses.
const (
	SourcePending   = "PENDING_REVIEW"
	SourceApproved  = "APPROVED"
	SourceSuspended = "SUSPENDED"
	SourceRetired   = "RETIRED"
)

// Duplicate kinds (2.3).
const (
	DupExact         = "EXACT"
	DupMirror        = "MIRROR"
	DupRepost        = "REPOST"
	DupVersion       = "VERSION"
	DupNearDuplicate = "NEAR_DUPLICATE"
)

// Maturity levels (shared with knowledge_documents.maturity).
const (
	MaturityExperimental = "EXPERIMENTAL"
	MaturityEmerging     = "EMERGING"
	MaturityMainstream   = "MAINSTREAM"
	MaturityLegacy       = "LEGACY"
	MaturityDeprecated   = "DEPRECATED"
)

// Report statuses.
const (
	ReportDraft      = "DRAFT"
	ReportValidated  = "VALIDATED"
	ReportRejected   = "REJECTED"
	ReportPublished  = "PUBLISHED"
	ReportSuperseded = "SUPERSEDED"
	ReportStale      = "STALE"
)

// KnowledgeKeyPrefix prefixes the canonical_key of every knowledge document Pulse publishes.
const KnowledgeKeyPrefix = "pulse:"

// Source is one trend_sources registry entry (2.1).
type Source struct {
	ID                  string         `json:"id"`
	Key                 string         `json:"source_key"`
	Name                string         `json:"name"`
	Connector           string         `json:"connector"`
	FeedURL             string         `json:"feed_url"`
	HomepageURL         string         `json:"homepage_url"`
	SourceType          string         `json:"source_type"`
	IsPrimarySource     bool           `json:"is_primary_source"`
	Reliability         float64        `json:"reliability"`
	TermsURL            string         `json:"terms_url"`
	TermsSummary        string         `json:"terms_summary"`
	Licence             string         `json:"licence"`
	AllowedContent      string         `json:"allowed_content"`
	MaxSummaryChars     int            `json:"max_summary_chars"`
	AttributionRequired bool           `json:"attribution_required"`
	RateLimitPerHour    int            `json:"rate_limit_per_hour"`
	MinIntervalSeconds  int            `json:"min_interval_seconds"`
	Config              map[string]any `json:"config,omitempty"`
	Status              string         `json:"status"`
	ApprovedBy          string         `json:"approved_by,omitempty"`
	ApprovedAt          *time.Time     `json:"approved_at,omitempty"`
	ETag                string         `json:"-"`
	LastModified        string         `json:"-"`
	LastFetchedAt       *time.Time     `json:"last_fetched_at,omitempty"`
	NextFetchAt         time.Time      `json:"next_fetch_at"`
	ConsecutiveFailures int            `json:"consecutive_failures"`
	LastError           string         `json:"last_error,omitempty"`
}

// RawItem is what a connector extracts from one feed entry, before policy and dedup.
type RawItem struct {
	ExternalID   string
	URL          string
	Title        string
	Summary      string
	Content      string
	Authors      []string
	PublishedAt  *time.Time
	SeriesKey    string // groups versions of one document
	VersionLabel string
	Engagement   int
	Hints        map[string]any // connector-specific signals (e.g. prerelease)
}

// Item is one stored trend_items row.
type Item struct {
	ID                string         `json:"id"`
	SourceID          string         `json:"source_id"`
	SourceKey         string         `json:"source_key,omitempty"`
	SourceName        string         `json:"source_name,omitempty"`
	ExternalID        string         `json:"external_id"`
	URL               string         `json:"url"`
	CanonicalURL      string         `json:"canonical_url"`
	Title             string         `json:"title"`
	Summary           string         `json:"summary,omitempty"`
	Authors           []string       `json:"authors,omitempty"`
	PublishedAt       *time.Time     `json:"published_at,omitempty"`
	RetrievedAt       time.Time      `json:"retrieved_at"`
	RetrievalMetadata map[string]any `json:"retrieval_metadata"`
	SeriesKey         string         `json:"series_key,omitempty"`
	VersionLabel      string         `json:"version_label,omitempty"`
	Engagement        int            `json:"engagement"`
	CanonicalItemID   string         `json:"canonical_item_id,omitempty"`
	DuplicateKind     string         `json:"duplicate_kind,omitempty"`
	DuplicateScore    *float64       `json:"duplicate_score,omitempty"`
	Maturity          string         `json:"maturity"`
	Significance      float64        `json:"significance"`
	Topics            []ItemTopic    `json:"topics,omitempty"`
	Duplicates        []Item         `json:"duplicates,omitempty"`
}

// ItemTopic is one classification of an item.
type ItemTopic struct {
	TopicKey     string   `json:"topic_key"`
	Facet        string   `json:"facet"`
	Confidence   float64  `json:"confidence"`
	MatchedTerms []string `json:"matched_terms"`
}

// Topic is one trend_topics taxonomy node.
type Topic struct {
	ID          string     `json:"id"`
	Key         string     `json:"topic_key"`
	Name        string     `json:"name"`
	Facet       string     `json:"facet"`
	ParentKey   string     `json:"parent_key,omitempty"`
	Keywords    []string   `json:"keywords"`
	Description string     `json:"description"`
	Maturity    string     `json:"maturity"`
	FirstSeenAt *time.Time `json:"first_seen_at,omitempty"`
}

// Observation is one day of signal for one topic.
type Observation struct {
	Day          time.Time `json:"day"`
	ItemCount    int       `json:"item_count"`
	MentionCount int       `json:"mention_count"`
	SourceCount  int       `json:"source_count"`
	PrimaryCount int       `json:"primary_count"`
	Engagement   int64     `json:"engagement"`
}

// TrendScore is one trend_scores row.
type TrendScore struct {
	ID         string         `json:"id"`
	TopicKey   string         `json:"topic_key"`
	ComputedAt time.Time      `json:"computed_at"`
	WindowDays int            `json:"window_days"`
	Components Components     `json:"components"`
	Score      float64        `json:"score"`
	Inputs     map[string]any `json:"inputs,omitempty"`
}

// Finding is one claim in a research report.
type Finding struct {
	Claim    string   `json:"claim"`
	Kind     string   `json:"kind"` // TECHNICAL | ADOPTION
	Evidence []string `json:"evidence"`
}

// Finding kinds: technical proof is kept separate from adoption signals (§6.5).
const (
	FindingTechnical = "TECHNICAL"
	FindingAdoption  = "ADOPTION"
)

// Report is one research_reports row with its evidence.
type Report struct {
	ID             string           `json:"id"`
	TopicID        string           `json:"-"`
	TopicKey       string           `json:"topic_key"`
	TrendScoreID   string           `json:"trend_score_id,omitempty"`
	Title          string           `json:"title"`
	Summary        string           `json:"summary"`
	Findings       []Finding        `json:"findings"`
	Limitations    []string         `json:"limitations"`
	Caveats        []string         `json:"caveats"`
	Confidence     float64          `json:"confidence"`
	ConfidenceBand string           `json:"confidence_band"`
	Maturity       string           `json:"maturity"`
	FreshnessAt    *time.Time       `json:"freshness_at,omitempty"`
	StaleAfter     time.Time        `json:"stale_after"`
	EvidenceHash   string           `json:"evidence_hash"`
	Status         string           `json:"status"`
	Validation     GateResult       `json:"validation"`
	ValidatedAt    *time.Time       `json:"validated_at,omitempty"`
	Evidence       []EvidenceItem   `json:"evidence"`
	CreatedAt      time.Time        `json:"created_at"`
	Knowledge      *KnowledgeUpdate `json:"knowledge_update,omitempty"`
}

// EvidenceItem is an item cited by a report, with the provenance the gate checks.
type EvidenceItem struct {
	ItemID          string         `json:"item_id"`
	Role            string         `json:"role"` // PRIMARY | CORROBORATING
	Title           string         `json:"title"`
	URL             string         `json:"url"`
	Summary         string         `json:"-"`
	PublishedAt     *time.Time     `json:"published_at,omitempty"`
	RetrievedAt     time.Time      `json:"retrieved_at"`
	FetchID         string         `json:"fetch_id"`
	IsCanonical     bool           `json:"is_canonical"`
	Maturity        string         `json:"maturity"`
	Significance    float64        `json:"significance"`
	SourceKey       string         `json:"source_key"`
	SourceName      string         `json:"source_name"`
	SourceType      string         `json:"source_type"`
	SourceStatus    string         `json:"source_status"`
	IsPrimarySource bool           `json:"is_primary_source"`
	Reliability     float64        `json:"reliability"`
	TermsURL        string         `json:"terms_url"`
	Licence         string         `json:"licence"`
	AllowedContent  string         `json:"allowed_content"`
	Retrieval       map[string]any `json:"retrieval_metadata,omitempty"`
}

// KnowledgeUpdate is one knowledge_updates row.
type KnowledgeUpdate struct {
	ID          string    `json:"id"`
	ReportID    string    `json:"report_id"`
	TopicKey    string    `json:"topic_key"`
	DocumentID  string    `json:"document_id"`
	Status      string    `json:"status"`
	PublishedAt time.Time `json:"published_at"`
	Title       string    `json:"title,omitempty"`
	Confidence  float64   `json:"confidence"`
	Band        string    `json:"confidence_band,omitempty"`
	Maturity    string    `json:"maturity,omitempty"`
	StaleAfter  time.Time `json:"stale_after"`
	Notified    []string  `json:"notified_agent_types,omitempty"`
}

// Subscription is one agent_knowledge_subscriptions row.
type Subscription struct {
	ID            string  `json:"id"`
	AgentType     string  `json:"agent_type"`
	TopicKey      string  `json:"topic_key"`
	MinConfidence float64 `json:"min_confidence"`
	Active        bool    `json:"active"`
}

// Notification is one agent_knowledge_notifications row.
type Notification struct {
	ID                string    `json:"id"`
	AgentType         string    `json:"agent_type"`
	KnowledgeUpdateID string    `json:"knowledge_update_id"`
	DocumentID        string    `json:"document_id"`
	TopicKey          string    `json:"topic_key"`
	Title             string    `json:"title"`
	CreatedAt         time.Time `json:"created_at"`
}
