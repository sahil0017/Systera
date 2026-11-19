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

export const COLLECTOR_VERSION = "1.1.0";
