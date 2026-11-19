export type XRayLayerId =
  | 'product_capability'
  | 'domain'
  | 'service'
  | 'api'
  | 'data'
  | 'events'
  | 'infrastructure'
  | 'security_reliability';

export type EvidenceState = 'OBSERVED' | 'INFERRED' | 'PROPOSED';
export type Direction = 'both' | 'out' | 'in';

export interface XRayLayerMeta {
  id: XRayLayerId;
  label: string;
  order: number;
  color: string;
  description: string;
  defaultNodeTypes: string[];
}

export interface XRayNode {
  id: string;
  key: string;
  type: string;
  name: string;
  description?: string;
  owner_domain?: string;
  lifecycle_state?: string;
  evidence_state: EvidenceState;
  layer: XRayLayerId;
  depth?: number;
  properties?: Record<string, unknown>;
}

export interface XRayEdge {
  id: string;
  source_node_id: string;
  target_node_id: string;
  relation_type: string;
  protocol?: string;
  sync_mode?: string;
  source_layer: XRayLayerId;
  target_layer: XRayLayerId;
  cross_layer: boolean;
}

export interface XRayLayerGroup {
  id: XRayLayerId;
  label: string;
  order: number;
  description?: string;
  node_count: number;
  edge_count: number;
  nodes: XRayNode[];
  edges: XRayEdge[];
}

export interface XRayView {
  model_id: string;
  version: number;
  graph_hash: string;
  total_nodes: number;
  total_edges: number;
  layers: XRayLayerGroup[];
  cross_layer_edges: XRayEdge[];
}

export interface XRayDrillDown {
  model_id: string;
  version: number;
  center_node_id: string;
  center_layer: XRayLayerId;
  depth: number;
  direction: Direction;
  truncated: boolean;
  layers: XRayLayerGroup[];
  cross_layer_edges: XRayEdge[];
}

export interface NodeRef {
  id: string;
  key: string;
  type: string;
  name: string;
  layer: XRayLayerId;
  evidence_state: EvidenceState;
}

export interface EdgeNodeRef {
  node: NodeRef;
  edge_id: string;
  relation_type: string;
  protocol?: string;
  sync_mode?: string;
}

export interface InterfaceSpec {
  role: 'EXPOSES' | 'CONSUMES' | 'PUBLISHES' | 'SUBSCRIBES';
  relation_type: string;
  protocol?: string;
  sync_mode?: string;
  target: NodeRef;
}

export interface InspectedEvidence {
  id: string;
  source_ref: string;
  source_type: string;
  directness: string;
  trust_level: string;
  excerpt?: string;
  captured_at?: string;
}

export interface InspectedClaim {
  id: string;
  statement: string;
  claim_class: EvidenceState;
  confidence_band: 'HIGH' | 'MEDIUM' | 'LOW';
  status: string;
  review_state: string;
  evidence: InspectedEvidence[];
}

export interface InspectedAssumption {
  id: string;
  statement: string;
  rationale: string;
  owner: string;
  risk_if_wrong: 'HIGH' | 'MEDIUM' | 'LOW';
  validation_method?: string;
  status: string;
}

export interface FailureMode {
  title: string;
  severity: 'HIGH' | 'MEDIUM' | 'LOW';
  source: 'GRAPH_THREAT' | 'GRAPH_SLO' | 'PROPERTY' | 'STRUCTURAL';
  description: string;
  mitigation?: string;
  related_node_id?: string;
}

export interface InspectedAlternative {
  source: string;
  decision_id?: string;
  adr_number?: number;
  title: string;
  chosen_option?: string;
  option: string;
  pros?: string;
  cons?: string;
  rejection_reason?: string;
  status?: string;
}

export interface ComponentInspection {
  model_id: string;
  version: number;
  node: XRayNode;
  responsibilities: string[];
  owned_components: NodeRef[];
  dependencies: {
    upstream: EdgeNodeRef[];
    downstream: EdgeNodeRef[];
  };
  interfaces: InterfaceSpec[];
  data_ownership: {
    owns: NodeRef[];
    reads: NodeRef[];
    writes: NodeRef[];
  };
  evidence: InspectedClaim[];
  assumptions: InspectedAssumption[];
  failure_modes: FailureMode[];
  alternatives: InspectedAlternative[];
}

// Section 7: Architecture Counterfactual types
export type ConstraintId =
  | 'scale'
  | 'availability'
  | 'latency'
  | 'cost'
  | 'team_size'
  | 'compliance'
  | 'operational_maturity'
  | 'migration_complexity';

export type AlternativeStyle =
  | 'modular_monolith'
  | 'domain_microservices'
  | 'event_driven_hybrid';

export interface ConstraintScore {
  constraint: ConstraintId;
  score: number;
  weight: number;
  rating: 'HIGH' | 'MEDIUM' | 'LOW';
  rationale: string;
}

export interface Alternative {
  id: string;
  set_id: string;
  style: AlternativeStyle;
  name: string;
  summary: string;
  when_appropriate: string;
  pros: string;
  cons: string;
  graph_version: number;
  is_recommended: boolean;
  scores: Record<ConstraintId, ConstraintScore>;
  weighted_score: number;
  decision_id?: string;
}

export interface DecisionMatrixRow {
  constraint: ConstraintId;
  weight: number;
  scores: Record<AlternativeStyle, ConstraintScore>;
  best_style: AlternativeStyle;
}

export interface ProjectSummary {
  id: string;
  name: string;
  description?: string;
}

export interface ArchitectureModel {
  id: string;
  project_id: string;
  current_version: number;
  status: string;
}

export interface GraphVersion {
  id: string;
  model_id: string;
  version: number;
  graph_hash: string;
  node_count: number;
  edge_count: number;
  change_summary: string;
}

export interface GraphDiff {
  model_id: string;
  from_version: number;
  to_version: number;
  nodes: {
    added: XRayNode[];
    removed: XRayNode[];
    modified: Array<{ id: string; key: string; changed_fields: string[] }>;
  };
  edges: {
    added: XRayEdge[];
    removed: XRayEdge[];
    modified: Array<{ id: string; changed_fields: string[] }>;
  };
}

export interface AlternativeDiff {
  model_id: string;
  from_alternative: Alternative;
  to_alternative: Alternative;
  score_deltas: Record<ConstraintId, number>;
  weighted_delta: number;
  graph_diff: GraphDiff;
}

export interface CounterfactualSet {
  set_id: string;
  model_id?: string;
  website_id?: string;
  subject: string;
  alternatives: Alternative[];
  decision_matrix: {
    constraints: ConstraintId[];
    rows: DecisionMatrixRow[];
    recommended_style: AlternativeStyle;
    recommended_reason: string;
  };
  decision_id?: string;
}
