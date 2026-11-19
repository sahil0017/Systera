export type AnalysisMode = "quick_scan" | "product_map" | "full_system_design" | "compare_websites";

export interface FormObservation {
  action?: string;
  method?: string;
  labels: string[];
}

export interface ResourceMetadata {
  name: string;
  initiator_type?: string;
  duration_ms?: number;
}

export interface ObservationPayload {
  headings: string[];
  navigation: string[];
  visible_features: string[];
  visible_text: string[];
  forms: FormObservation[];
  public_routes: string[];
  public_metadata: Record<string, string>;
  resource_metadata: ResourceMetadata[];
}

export interface NormalizedObservation {
  schema_version: "1.0";
  collector_version: string;
  url: string;
  title: string;
  user_initiated: true;
  observations: ObservationPayload;
  redactions_applied: string[];
}

export interface Session {
  id: string;
  domain: string;
  analysis_mode: AnalysisMode;
  allowed_pages: string[];
  max_pages: number;
  consent_version: string;
  retention_expires_at: string;
  status: string;
  observation_count: number;
}

export interface StoredObservation {
  id: string;
  session_id: string;
  url: string;
  title: string;
  observations: ObservationPayload;
  redactions_applied: string[];
  injection_suspected: boolean;
  payload_bytes: number;
  captured_at: string;
}

export interface CapabilityFinding {
  name: string;
  evidence_class: "OBSERVED" | "INFERRED" | "PROPOSED";
  evidence_refs: string[];
  confidence: string;
  rationale: string;
}

export interface WebsiteProfile {
  id: string;
  session_id: string;
  domain: string;
  project_id?: string;
  model_id?: string;
  capabilities: CapabilityFinding[];
  journeys: string[];
  tech_signals?: CapabilityFinding[];
  run_id?: string;
  created_at?: string;
  fingerprint: Record<string, unknown>;
  alternatives: Array<{ name: string; summary: string; strengths: string[]; risks: string[]; best_when: string }>;
  status: string;
}

export interface DeletionReport {
  session_id?: string;
  observations_deleted: number;
  derived_profiles_deleted: number;
  status: string;
}

export interface Consent {
  consent_version: string;
  text: string;
  max_pages: number;
  default_retention_days: number;
  max_retention_days: number;
}

export type EvidenceClass = "OBSERVED" | "INFERRED" | "PROPOSED";

export interface Finding {
  id: string;
  kind: string; // CAPABILITY | JOURNEY | TECHNOLOGY_SIGNAL | SYSTEM_DESIGN | DOMAIN | COMPONENT | DATA_ENTITY | API | INFRASTRUCTURE
  name: string;
  statement: string;
  evidence_class: EvidenceClass;
  confidence: string;
  rationale: string;
  observation_ids: string[];
  source: "BASELINE" | "AGENT";
  agent_type?: string;
  clue?: string;
}

export interface FingerprintElement {
  node_id: string;
  name: string;
  node_type: string;
  evidence_class: EvidenceClass;
  confidence: string;
}

export interface Fingerprint {
  website_id: string;
  domain: string;
  disclaimer: string;
  layers: Record<string, { layer: string; elements: FingerprintElement[] | null; edges: unknown[] | null }>;
  quality_attributes: Array<{ attribute: string; target: string; evidence_class: EvidenceClass; confidence: string; rationale: string }> | null;
}

export interface ConstraintScore {
  constraint: string;
  score: number;
  rating: string;
  rationale: string;
}

export interface Alternative {
  id: string;
  style: string;
  name: string;
  summary: string;
  when_appropriate: string;
  pros: string;
  cons: string;
  is_recommended: boolean;
  weighted_score: number;
  scores: Record<string, ConstraintScore>;
}

export interface CounterfactualSet {
  set_id: string;
  alternatives: Alternative[];
  decision_matrix: { constraints: string[]; recommended_style: string; recommended_reason: string };
}

export interface AnalysisStart {
  website: WebsiteProfile;
  run: { id: string; state: string };
  tasks: Array<{ id: string; key: string; agent_type: string; state: string }>;
}

export interface ComparedSite {
  website_id: string;
  domain: string;
  analysis_mode: AnalysisMode;
  page_count: number;
}

export interface WebsiteComparison {
  taxonomy_version: string;
  disclaimer: string;
  sites: ComparedSite[];
  capabilities: Array<{ key: string; name: string; proposed_component: string; present_on: number; sites: Record<string, { present: boolean; evidence_class?: EvidenceClass; confidence?: string }> }>;
  shared_capabilities: string[];
  unique_capabilities: Record<string, string[]>;
  unmapped_capabilities: Record<string, string[]>;
  journeys: Record<string, string[]>;
  technology: Array<{ name: string; sites: Record<string, string> }>;
  pairs: Array<{ a: string; b: string; capability_similarity: number; shared_capabilities: string[]; shared_technology: string[] }>;
  notes: string[];
}

export const COLLECTOR_VERSION = "1.1.0";
