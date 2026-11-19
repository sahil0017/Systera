import type {
  Direction,
  XRayEdge,
  XRayLayerGroup,
  XRayLayerId,
  XRayLayerMeta,
  XRayNode,
  XRayView,
  XRayDrillDown,
} from '../types/xray';

export const XRAY_LAYERS: XRayLayerMeta[] = [
  {
    id: 'product_capability',
    label: 'Product / Capability',
    order: 1,
    color: '#38bdf8',
    description: 'User-facing product capabilities, actors, systems, and workloads.',
    defaultNodeTypes: ['system', 'actor', 'workload'],
  },
  {
    id: 'domain',
    label: 'Domain',
    order: 2,
    color: '#818cf8',
    description: 'Bounded contexts, business domains, and ownership boundaries.',
    defaultNodeTypes: ['domain'],
  },
  {
    id: 'service',
    label: 'Service',
    order: 3,
    color: '#34d399',
    description: 'Runtime services, internal components, and external dependencies.',
    defaultNodeTypes: ['service', 'component', 'external_dependency'],
  },
  {
    id: 'api',
    label: 'API',
    order: 4,
    color: '#fbbf24',
    description: 'Synchronous and asynchronous API endpoints and contracts.',
    defaultNodeTypes: ['api'],
  },
  {
    id: 'data',
    label: 'Data',
    order: 5,
    color: '#f472b6',
    description: 'Databases, tables, schemas, and caches.',
    defaultNodeTypes: ['database', 'table', 'cache'],
  },
  {
    id: 'events',
    label: 'Events',
    order: 6,
    color: '#fb923c',
    description: 'Domain events, message topics, and work queues.',
    defaultNodeTypes: ['event', 'topic', 'queue'],
  },
  {
    id: 'infrastructure',
    label: 'Infrastructure',
    order: 7,
    color: '#94a3b8',
    description: 'Deployment units, compute clusters, and cloud regions.',
    defaultNodeTypes: ['deployment_unit', 'region'],
  },
  {
    id: 'security_reliability',
    label: 'Security / Reliability',
    order: 8,
    color: '#f87171',
    description: 'Secret boundaries, SLOs, threats, and verification tests.',
    defaultNodeTypes: ['secret_boundary', 'slo', 'threat', 'test'],
  },
];

export const LAYER_BY_ID: Record<XRayLayerId, XRayLayerMeta> = Object.fromEntries(
  XRAY_LAYERS.map((l) => [l.id, l]),
) as Record<XRayLayerId, XRayLayerMeta>;

export interface LocalDrillDownOptions {
  depth?: number;
  direction?: Direction;
  targetLayers?: XRayLayerId[];
}

/**
 * Local BFS neighbourhood drill-down across the 8 X-Ray layers (mirrors
 * GET /v1/architecture/{model}/xray/nodes/{node}/drilldown).
 */
export function computeLocalDrillDown(
  view: XRayView,
  centerNodeID: string,
  { depth = 2, direction = 'both', targetLayers }: LocalDrillDownOptions = {},
): XRayDrillDown | null {
  const allNodes = new Map<string, XRayNode>();
  const allEdges: XRayEdge[] = [];
  for (const layer of view.layers) {
    for (const n of layer.nodes) allNodes.set(n.id, { ...n, layer: layer.id });
    for (const e of layer.edges) allEdges.push(e);
  }
  for (const e of view.cross_layer_edges) allEdges.push(e);

  const centerNode = allNodes.get(centerNodeID);
  if (!centerNode) return null;

  const depths = new Map<string, number>([[centerNodeID, 0]]);
  let frontier: string[] = [centerNodeID];

  for (let d = 1; d <= depth && frontier.length > 0; d++) {
    const next: string[] = [];
    const frontierSet = new Set(frontier);
    for (const e of allEdges) {
      if ((direction === 'both' || direction === 'out') && frontierSet.has(e.source_node_id)) {
        if (!depths.has(e.target_node_id)) {
          depths.set(e.target_node_id, d);
          next.push(e.target_node_id);
        }
      }
      if ((direction === 'both' || direction === 'in') && frontierSet.has(e.target_node_id)) {
        if (!depths.has(e.source_node_id)) {
          depths.set(e.source_node_id, d);
          next.push(e.source_node_id);
        }
      }
    }
    frontier = next;
  }

  const allowed = targetLayers && targetLayers.length > 0 ? new Set(targetLayers) : null;
  const keptIds = new Set<string>();
  for (const [id] of depths) {
    const n = allNodes.get(id)!;
    if (!allowed || allowed.has(n.layer) || id === centerNodeID) {
      keptIds.add(id);
    }
  }

  const keptEdges = allEdges.filter((e) => keptIds.has(e.source_node_id) && keptIds.has(e.target_node_id));
  const layers: XRayLayerGroup[] = XRAY_LAYERS.map((meta) => {
    const nodes = [...keptIds]
      .map((id) => ({ ...allNodes.get(id)!, depth: depths.get(id) }))
      .filter((n) => n.layer === meta.id)
      .sort((a, b) => (a.depth ?? 0) - (b.depth ?? 0) || a.key.localeCompare(b.key));
    const edges = keptEdges.filter((e) => !e.cross_layer && e.source_layer === meta.id);
    return {
      id: meta.id,
      label: meta.label,
      order: meta.order,
      description: meta.description,
      node_count: nodes.length,
      edge_count: edges.length,
      nodes,
      edges,
    };
  }).filter((g) => !allowed || allowed.has(g.id) || g.node_count > 0);

  return {
    model_id: view.model_id,
    version: view.version,
    center_node_id: centerNodeID,
    center_layer: centerNode.layer,
    depth,
    direction,
    truncated: false,
    layers,
    cross_layer_edges: keptEdges.filter((e) => e.cross_layer),
  };
}
