// Design-time Architecture Digital Twin (control-plane internal/twin), spec §23.4 / §26.

export type ValueSource = 'DECLARED' | 'ASSUMED' | 'DEFAULT' | 'MISSING';
export type FailureKind = 'NODE_LOSS' | 'ZONE_LOSS' | 'REGION_LOSS' | 'DEPENDENCY_OUTAGE' | 'NETWORK_PARTITION' | 'LATENCY_SPIKE';
export type Dimension = 'STRUCTURE' | 'INTERFACES' | 'DATA' | 'RUNTIME' | 'BEHAVIOUR' | 'QUALITY_ATTRIBUTES';
export type CheckStatus = 'PASS' | 'WARN' | 'FAIL' | 'NOT_APPLICABLE';
export type Severity = 'ERROR' | 'WARNING' | 'INFO';
export type ReportStatus = 'VALID' | 'VALID_WITH_WARNINGS' | 'INVALID';

export interface TwinAssumptions {
  workload: { peak_rps: number; read_ratio?: number; avg_payload_kb?: number; growth_pct_per_month?: number };
  environment: { cloud?: string; regions: string[] | null; default_replicas?: number; default_timeout_ms?: number };
  failures: Array<{ kind: FailureKind; target_node_id?: string; note?: string }> | null;
  notes?: string;
}

export interface TwinNodeRef { id: string; key: string; name: string; type: string }

export interface TwinFinding {
  severity: Severity;
  message: string;
  subjects: TwinNodeRef[] | null;
  edge_ids?: string[];
  relies_on_defaults?: boolean;
}

export interface TwinCheck {
  id: string;
  dimension: Dimension;
  title: string;
  status: CheckStatus;
  detail?: string;
  findings: TwinFinding[] | null;
  remediation?: string;
}

export interface DimensionSummary {
  dimension: Dimension;
  status: CheckStatus;
  elements: number;
  passed: number;
  warned: number;
  failed: number;
  not_applicable: number;
}

export interface TwinReport {
  status: ReportStatus;
  basis: string;
  disclaimer: string;
  error_count: number;
  warning_count: number;
  dimensions: DimensionSummary[];
  checks: TwinCheck[];
  limitations: string[];
}

export interface RuntimeUnit extends TwinNodeRef {
  replicas: number;
  replicas_source: ValueSource;
  capacity_rps_per_replica?: number;
  capacity_source: ValueSource;
  placed_on: TwinNodeRef[];
  regions: string[];
}

export interface Interaction {
  edge_id: string;
  from: TwinNodeRef;
  to: TwinNodeRef;
  relation: string;
  protocol?: string;
  sync_mode: string;
  sync_source: ValueSource;
  timeout_ms?: number;
  timeout_source: ValueSource;
  retries: number;
  retries_source: ValueSource;
  backoff: boolean;
  circuit_breaker: boolean;
}

export interface RequestPath {
  entry: TwinNodeRef;
  nodes: TwinNodeRef[];
  edge_ids: string[];
  sync_hops: number;
  timeout_budget_ms?: number;
  retry_amplification: number;
  ends_at: 'DATA' | 'ASYNC' | 'LEAF' | 'CYCLE' | 'DEPTH_LIMIT';
  covered_by_slo_ids: string[];
}

export interface Twin {
  model_id: string;
  graph_version: number;
  graph_hash: string;
  basis: string;
  disclaimer: string;
  builder_version: string;
  assumptions_hash: string;
  assumptions: TwinAssumptions;
  structure: {
    components: Array<TwinNodeRef & { owner?: string; owner_source: string; critical: boolean; lifecycle_state: string; evidence_state: string; depends_on: string[]; dependent_count: number }>;
    boundaries: TwinNodeRef[];
    edge_count: number;
  };
  interfaces: Array<TwinNodeRef & { kind: 'API' | 'EVENT' | 'CHANNEL'; protocols: string[]; version?: string; schema_ref?: string; providers: TwinNodeRef[]; consumers: TwinNodeRef[]; contract_basis: string }>;
  data: Array<TwinNodeRef & { writers: TwinNodeRef[]; readers: TwinNodeRef[]; owner?: TwinNodeRef; consistency?: string; retention?: string; replicates_to: TwinNodeRef[]; fails_over_to: TwinNodeRef[] }>;
  runtime: { units: RuntimeUnit[]; deployment_units: TwinNodeRef[]; regions: string[]; regions_source: ValueSource; topology_modeled: boolean };
  behaviour: { interactions: Interaction[]; entry_points: TwinNodeRef[]; request_paths: RequestPath[]; paths_capped: boolean };
  quality: {
    slos: Array<TwinNodeRef & { availability_pct?: number; latency_p95_ms?: number; latency_p99_ms?: number; rto_minutes?: number; rpo_minutes?: number; covers: TwinNodeRef[] }>;
    paths_covered: number;
    paths_total: number;
    coverage_pct: number;
    runtime_units_covered: number;
    runtime_units_on_paths: number;
    strictest_availability_pct?: number;
    has_rto_or_rpo: boolean;
  };
  defaults_applied: Array<{ subject: TwinNodeRef; field: string; value: unknown; source: ValueSource; reason: string }>;
}

export interface TwinBuild {
  id?: string; // absent on a dry run
  model_id: string;
  graph_version: number;
  graph_hash: string;
  builder_version: string;
  assumptions_hash: string;
  basis: string;
  status: ReportStatus;
  error_count: number;
  warning_count: number;
  assumptions: TwinAssumptions;
  created_by?: string;
  created_at?: string;
  twin?: Twin;
  report?: TwinReport;
}
