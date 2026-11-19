// Engineering Pulse, knowledge/memory, website investigation and run shapes, mirroring the
// control plane's JSON (internal/engineeringpulse, internal/knowledge, internal/browserintel).

export type Facet = 'DOMAIN' | 'TECHNOLOGY' | 'PATTERN';

export interface Topic {
  id: string;
  topic_key: string;
  name: string;
  facet: Facet;
  parent_key?: string;
  keywords: string[] | null;
  description: string;
  maturity: string;
  first_seen_at?: string;
}

export interface TrendComponents {
  popularity: number;
  growth: number;
  significance: number;
  reliability: number;
  novelty: number;
}

export interface TrendScore {
  id: string;
  topic_key: string;
  computed_at: string;
  window_days: number;
  components: TrendComponents;
  score: number;
}

export interface ReportBrief {
  id: string;
  status: string;
  confidence: number;
  confidence_band: string;
  failed_checks?: string[];
}

export interface KnowledgeUpdate {
  id: string;
  report_id: string;
  topic_key: string;
  document_id: string;
  status: string;
  published_at: string;
  title?: string;
  confidence: number;
  confidence_band?: string;
  maturity?: string;
  stale_after: string;
  notified_agent_types?: string[];
}

export interface TrendSummary {
  topic: Topic;
  score: TrendScore;
  latest_report?: ReportBrief;
  knowledge?: KnowledgeUpdate;
}

export interface TrendList {
  items: TrendSummary[];
  note?: string;
}

export interface PulseItem {
  id: string;
  source_key?: string;
  source_name?: string;
  url: string;
  title: string;
  summary?: string;
  authors?: string[];
  published_at?: string;
  retrieved_at: string;
  engagement: number;
  maturity: string;
  significance: number;
  duplicate_kind?: string;
}

export interface Observation {
  day: string;
  item_count: number;
  mention_count: number;
  source_count: number;
  primary_count: number;
  engagement: number;
}

export interface ReportFinding {
  claim: string;
  kind: string;
  evidence: string[] | null;
}

export interface ReportEvidence {
  item_id: string;
  role: string;
  title: string;
  url: string;
  published_at?: string;
  retrieved_at: string;
  maturity: string;
  source_name: string;
  source_type: string;
  is_primary_source: boolean;
  reliability: number;
  licence: string;
  terms_url: string;
}

export interface GateCheck {
  name: string;
  passed: boolean;
  detail?: string;
}

export interface Report {
  id: string;
  topic_key: string;
  title: string;
  summary: string;
  findings: ReportFinding[] | null;
  limitations: string[] | null;
  caveats: string[] | null;
  confidence: number;
  confidence_band: string;
  maturity: string;
  freshness_at?: string;
  stale_after: string;
  status: string;
  validation: { passed: boolean; checks: GateCheck[] | null; validated_at: string };
  evidence: ReportEvidence[] | null;
  created_at: string;
  knowledge_update?: KnowledgeUpdate;
}

export interface TopicDetail {
  topic: Topic;
  scores: TrendScore[] | null;
  observations: Observation[] | null;
  items: PulseItem[] | null;
  reports: Report[] | null;
  knowledge: KnowledgeUpdate[] | null;
}

export interface PulseSource {
  id: string;
  source_key: string;
  name: string;
  connector: string;
  homepage_url: string;
  source_type: string;
  is_primary_source: boolean;
  reliability: number;
  terms_url: string;
  terms_summary: string;
  licence: string;
  allowed_content: string;
  attribution_required: boolean;
  rate_limit_per_hour: number;
  status: string;
  last_fetched_at?: string;
  consecutive_failures: number;
  last_error?: string;
}

export interface Notification {
  id: string;
  agent_type: string;
  knowledge_update_id: string;
  document_id: string;
  topic_key: string;
  title: string;
  created_at: string;
}

export interface Subscription {
  id: string;
  agent_type: string;
  topic_key: string;
  min_confidence: number;
  active: boolean;
}

export interface SupportingSource {
  item_id: string;
  title: string;
  url: string;
  source_name: string;
  role: string;
  is_primary_source: boolean;
  reliability: number;
  licence: string;
}

export interface Explanation {
  why: string[] | null;
  matched_terms: string[] | null;
  hybrid_score: number;
  keyword_score: number;
  vector_score: number;
  subscribed_domain?: string;
  supporting_sources: SupportingSource[] | null;
  limitations: string[] | null;
  caveats: string[] | null;
  confidence: number;
  confidence_band: string;
  maturity: string;
  freshness_at?: string;
  stale_after: string;
  status: string;
  adoption_signal: { trend_score?: number; selection_criterion: boolean; note: string };
}

export interface RunReference {
  id: string;
  run_id: string;
  task_id?: string;
  agent_type?: string;
  knowledge_update_id: string;
  document_id: string;
  topic_key: string;
  relevance_score: number;
  explanation: Explanation;
  created_at: string;
}

export interface OptionAssessment {
  name: string;
  maturity: string;
  operational_burden: number;
  cost: number;
  security: number;
  fit: number;
  rationale: Record<string, string>;
  trend_score?: number;
}

export interface RankedOption {
  rank: number;
  name: string;
  engineering_score: number;
  criterion_scores: Record<string, number>;
  trend_score?: number;
  tie_broken_by_trend?: boolean;
  excluded?: boolean;
  exclusion_reason?: string;
}

export interface OptionComparison {
  recommended: string;
  selection_basis: string;
  trend_score_role: string;
  weights: Record<string, number>;
  options: RankedOption[];
  warnings?: string[];
}

export interface KnowledgeResult {
  document_id: string;
  chunk_id: string;
  provenance_ref: string;
  title: string;
  source_url: string;
  source_type: string;
  is_primary_source: boolean;
  licence: string;
  published_at?: string;
  maturity: string;
  confidence_band: string;
  status: string;
  conflicts_with?: string[];
  conflict_note?: string;
  score: number;
  keyword_score: number;
  vector_score: number;
  injection_suspected: boolean;
  untrusted: boolean;
  content: string;
}

export interface KnowledgeDocument {
  id: string;
  canonical_key: string;
  version_number: number;
  title: string;
  source_url: string;
  source_type: string;
  is_primary_source: boolean;
  licence: string;
  access_policy: string;
  published_at?: string;
  maturity: string;
  confidence_band: string;
  status: string;
  superseded_by?: string;
  content_hash: string;
  created_at: string;
  updated_at: string;
}

export type SourceType = 'PRIMARY_DOC' | 'RFC' | 'RELEASE_NOTES' | 'ENGINEERING_BLOG' | 'PAPER' | 'COMMUNITY' | 'INTERNAL';
export type AccessPolicy = 'PUBLIC' | 'TENANT' | 'RESTRICTED';
export type MemoryType = 'WORKING' | 'EPISODIC' | 'SEMANTIC' | 'PROJECT';

export interface KnowledgeIngest {
  canonical_key?: string;
  title: string;
  source_url?: string;
  source_type: SourceType;
  is_primary_source: boolean;
  licence?: string;
  access_policy: AccessPolicy;
  published_at?: string;
  maturity: string;
  confidence_band: 'HIGH' | 'MEDIUM' | 'LOW';
  raw_content: string;
  collector?: string;
}

export interface MemoryInput {
  memory_type: MemoryType;
  summary: string;
  source_refs?: string[];
  run_id?: string;
  relevance_score?: number;
  access_policy?: 'PROJECT' | 'TENANT';
  expires_at?: string;
}

export interface MemoryRecord {
  id: string;
  project_id?: string;
  run_id?: string;
  memory_type: MemoryType;
  summary: string;
  content?: Record<string, unknown>;
  source_refs: string[] | null;
  relevance_score: number;
  access_policy: string;
  expires_at?: string;
  created_at: string;
}

// ---- Website investigations (Engineering Lens results) ----

export type EvidenceClass = 'OBSERVED' | 'INFERRED' | 'PROPOSED';

export interface CapabilityFinding {
  name: string;
  evidence_class: EvidenceClass;
  evidence_refs: string[] | null;
  confidence: string;
  rationale: string;
}

export interface WebsiteProfile {
  id: string;
  session_id: string;
  domain: string;
  project_id?: string;
  model_id?: string;
  run_id?: string;
  capabilities: CapabilityFinding[] | null;
  journeys: string[] | null;
  tech_signals: CapabilityFinding[] | null;
  alternatives: Array<{ name: string; summary: string; strengths: string[] | null; risks: string[] | null; best_when: string }> | null;
  source_observation_ids: string[] | null;
  status: string;
  promoted_at?: string;
  created_at: string;
  updated_at: string;
}

export interface WebsiteFinding {
  id: string;
  kind: string;
  name: string;
  statement: string;
  evidence_class: EvidenceClass;
  confidence: string;
  rationale: string;
  observation_ids: string[] | null;
  source: 'BASELINE' | 'AGENT';
  agent_type?: string;
}

export interface EvidenceLink {
  id: string;
  finding_type: string;
  finding_id: string;
  observation_id?: string;
  evidence_id?: string;
  claim_id?: string;
  claim_class: string;
  relation: string;
  statement: string;
  confidence: string;
}

export interface FingerprintElement {
  node_id: string;
  name: string;
  node_type: string;
  evidence_class: EvidenceClass;
  confidence: string;
}

export interface WebsiteFingerprint {
  website_id: string;
  domain: string;
  disclaimer: string;
  model_id?: string;
  graph_version?: number;
  layers: Record<string, { layer: string; elements: FingerprintElement[] | null }>;
  quality_attributes: Array<{ attribute: string; target: string; evidence_class: EvidenceClass; confidence: string; rationale: string }> | null;
}

export interface ComparedSite {
  website_id: string;
  domain: string;
  analysis_mode: string;
  page_count: number;
  status: string;
  model_id?: string;
}

export interface WebsiteComparison {
  taxonomy_version: string;
  disclaimer: string;
  sites: ComparedSite[];
  capabilities: Array<{
    key: string;
    name: string;
    proposed_component: string;
    present_on: number;
    sites: Record<string, { present: boolean; evidence_class?: EvidenceClass; confidence?: string }>;
  }> | null;
  shared_capabilities: string[] | null;
  technology: Array<{ name: string; sites: Record<string, string> }> | null;
  pairs: Array<{ a: string; b: string; capability_similarity: number; shared_capabilities: string[] | null; shared_technology: string[] | null }> | null;
  notes: string[] | null;
}

export interface DesignRun {
  id: string;
  project_id: string;
  objective: string;
  state: string;
  constraints: Record<string, unknown>;
  max_budget_usd: number;
  spent_budget_usd: number;
  failure_reason?: string;
  created_at: string;
  updated_at: string;
}

export interface RunTask {
  id: string;
  key: string;
  agent_type: string;
  objective: string;
  state: string;
  attempt_count: number;
  spent_usd: number;
  last_error?: string;
  completed_at?: string;
}

// ---- Learning overview ----

export interface LearningOverview {
  model_id: string;
  graph_version: number;
  disclaimer: string;
  components: Array<{ node_id: string; name: string; type: string; layer: string; one_liner: string }> | null;
  interview_questions: Array<{ level: string; question: string; good_answers_cover: string[] | null }> | null;
  exercises: Array<{ title: string; difficulty: string; goal: string; steps: string[] | null; acceptance_criteria: string[] | null }> | null;
}
