// Model insights computed from the X-Ray view the workspace already loaded: no extra requests.
// They point at where to look first; they are heuristics over the graph, not verdicts.
import type { XRayEdge, XRayLayerId, XRayNode, XRayView } from '../types/xray';

export interface Hotspot {
  node: XRayNode;
  value: number;
  detail: string;
}

export interface ModelInsights {
  nodes: number;
  edges: number;
  evidence: { OBSERVED: number; INFERRED: number; PROPOSED: number };
  layers: Array<{ id: XRayLayerId; label: string; count: number }>;
  mostDependedOn: Hotspot[];
  singlePoints: Hotspot[];
  syncChain: { nodes: XRayNode[]; length: number } | null;
  orphans: XRayNode[];
  score: number; // 0..100 "evidence coverage": share of components that are not only proposed
  headline: string;
}

const DEPENDS = new Set(['calls', 'reads', 'writes', 'depends_on', 'subscribes', 'authenticates']);
const RESILIENCE = new Set(['replicates', 'fails_over_to']);
const STRUCTURAL_TYPES = new Set(['system', 'domain', 'actor', 'slo', 'threat', 'test', 'workload', 'region']);

export function allEdges(view: XRayView): XRayEdge[] {
  return [...view.cross_layer_edges, ...view.layers.flatMap((l) => l.edges)];
}

export function computeInsights(view: XRayView): ModelInsights {
  const nodes = view.layers.flatMap((l) => l.nodes);
  const byId = new Map(nodes.map((n) => [n.id, n]));
  const edges = allEdges(view);
  const evidence = { OBSERVED: 0, INFERRED: 0, PROPOSED: 0 };
  for (const n of nodes) if (n.evidence_state in evidence) evidence[n.evidence_state]++;

  const dependents = new Map<string, Set<string>>();
  const resilient = new Set<string>();
  const degree = new Map<string, number>();
  for (const e of edges) {
    degree.set(e.source_node_id, (degree.get(e.source_node_id) ?? 0) + 1);
    degree.set(e.target_node_id, (degree.get(e.target_node_id) ?? 0) + 1);
    if (DEPENDS.has(e.relation_type)) {
      const s = dependents.get(e.target_node_id) ?? new Set<string>();
      s.add(e.source_node_id);
      dependents.set(e.target_node_id, s);
    }
    if (RESILIENCE.has(e.relation_type)) {
      resilient.add(e.source_node_id);
      resilient.add(e.target_node_id);
    }
  }

  const mostDependedOn = [...dependents.entries()]
    .map(([id, s]) => ({ node: byId.get(id)!, value: s.size }))
    .filter((h) => h.node && h.value >= 2)
    .sort((a, b) => b.value - a.value || a.node.name.localeCompare(b.node.name))
    .slice(0, 5)
    .map((h) => ({ ...h, detail: `${h.value} components depend on it` }));

  const singlePoints = mostDependedOn
    .filter((h) => !resilient.has(h.node.id) && !STRUCTURAL_TYPES.has(h.node.type))
    .map((h) => ({ ...h, detail: `${h.value} dependents, no replica or failover recorded` }));

  // Longest chain of synchronous calls (a latency and availability multiplier).
  const syncOut = new Map<string, string[]>();
  for (const e of edges) {
    if (e.relation_type === 'calls' && (e.sync_mode ?? 'SYNC').toUpperCase() === 'SYNC') {
      syncOut.set(e.source_node_id, [...(syncOut.get(e.source_node_id) ?? []), e.target_node_id]);
    }
  }
  const memo = new Map<string, string[]>();
  const longest = (id: string, stack: Set<string>): string[] => {
    if (memo.has(id)) return memo.get(id)!;
    if (stack.has(id)) return [id];
    stack.add(id);
    let best: string[] = [];
    for (const nx of syncOut.get(id) ?? []) {
      const p = longest(nx, stack);
      if (p.length > best.length) best = p;
    }
    stack.delete(id);
    const out = [id, ...best];
    memo.set(id, out);
    return out;
  };
  let chain: string[] = [];
  for (const id of syncOut.keys()) {
    const p = longest(id, new Set());
    if (p.length > chain.length) chain = p;
  }
  const syncChain = chain.length >= 3 ? { nodes: chain.map((id) => byId.get(id)!).filter(Boolean), length: chain.length - 1 } : null;

  const orphans = nodes.filter((n) => !degree.has(n.id) && !STRUCTURAL_TYPES.has(n.type)).slice(0, 8);
  const score = nodes.length === 0 ? 0 : Math.round(((evidence.OBSERVED + evidence.INFERRED) / nodes.length) * 100);

  const parts: string[] = [];
  if (singlePoints.length > 0) parts.push(`${singlePoints.length} likely single point${singlePoints.length === 1 ? '' : 's'} of failure`);
  if (syncChain) parts.push(`a ${syncChain.length}-hop synchronous call chain`);
  if (orphans.length > 0) parts.push(`${orphans.length} unconnected component${orphans.length === 1 ? '' : 's'}`);
  const headline = nodes.length === 0
    ? 'This model has no components yet.'
    : parts.length > 0 ? `Look first at ${parts.join(', ')}.` : 'No structural hotspots found in this version.';

  return {
    nodes: nodes.length,
    edges: edges.length,
    evidence,
    layers: view.layers.map((l) => ({ id: l.id, label: l.label, count: l.node_count })),
    mostDependedOn,
    singlePoints,
    syncChain,
    orphans,
    score,
    headline,
  };
}

/** Fuzzy match for the command palette: all query characters in order, word starts weigh more. */
export function fuzzyScore(query: string, text: string): number {
  const q = query.toLowerCase().trim();
  const t = text.toLowerCase();
  if (!q) return 1;
  if (t.includes(q)) return 100 - t.indexOf(q) + (t.startsWith(q) ? 50 : 0);
  let ti = 0;
  let score = 0;
  for (const ch of q) {
    const found = t.indexOf(ch, ti);
    if (found < 0) return 0;
    score += found === 0 || /[\s._:/-]/.test(t[found - 1]) ? 5 : 1;
    ti = found + 1;
  }
  return score;
}
