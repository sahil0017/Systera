import { describe, expect, it } from 'vitest';
import { computeInsights, fuzzyScore } from '../insights';
import { inspectToken } from '../session';
import type { XRayEdge, XRayNode, XRayView } from '../../types/xray';

const n = (id: string, type = 'service', layer: XRayNode['layer'] = 'service', ev: XRayNode['evidence_state'] = 'PROPOSED'): XRayNode =>
  ({ id, key: id, type, name: id.toUpperCase(), evidence_state: ev, layer });
const e = (s: string, t: string, rel = 'calls', sync = 'SYNC'): XRayEdge =>
  ({ id: `${s}-${t}-${rel}`, source_node_id: s, target_node_id: t, relation_type: rel, sync_mode: sync, source_layer: 'service', target_layer: 'service', cross_layer: false });
const view = (nodes: XRayNode[], edges: XRayEdge[]): XRayView => ({
  model_id: 'm', version: 1, graph_hash: 'h', total_nodes: nodes.length, total_edges: edges.length, cross_layer_edges: [],
  layers: [{ id: 'service', label: 'Service', order: 3, node_count: nodes.length, edge_count: edges.length, nodes, edges }],
});

describe('model insights', () => {
  it('finds single points of failure, sync chains and orphans', () => {
    const v = view(
      [n('a', 'service', 'service', 'OBSERVED'), n('b'), n('c'), n('db', 'database'), n('db2', 'database'), n('lonely')],
      [e('a', 'b'), e('b', 'c'), e('c', 'db', 'writes'), e('a', 'db', 'reads'), e('b', 'db', 'writes'), e('a', 'c')],
    );
    const i = computeInsights(v);
    expect(i.singlePoints.map((h) => h.node.id)).toContain('db');
    expect(i.mostDependedOn[0].node.id).toBe('db');
    expect(i.syncChain?.nodes.map((x) => x.id)).toEqual(['a', 'b', 'c']);
    expect(i.orphans.map((x) => x.id)).toEqual(['db2', 'lonely']);
    expect(i.score).toBe(17);
    expect(i.headline).toMatch(/single point/);
  });

  it('does not flag a component with a recorded replica', () => {
    const v = view([n('a'), n('b'), n('db', 'database'), n('dbr', 'database')],
      [e('a', 'db', 'reads'), e('b', 'db', 'reads'), e('db', 'dbr', 'replicates')]);
    expect(computeInsights(v).singlePoints).toEqual([]);
  });

  it('survives cycles of synchronous calls', () => {
    const v = view([n('a'), n('b'), n('c')], [e('a', 'b'), e('b', 'c'), e('c', 'a')]);
    expect(() => computeInsights(v)).not.toThrow();
  });
});

describe('fuzzy search', () => {
  it('ranks substring and word-start matches above scattered ones', () => {
    expect(fuzzyScore('order', 'Order Service')).toBeGreaterThan(fuzzyScore('order', 'Payment Orchestrator Delivery'));
    expect(fuzzyScore('os', 'Order Service')).toBeGreaterThan(0);
    expect(fuzzyScore('zz', 'Order Service')).toBe(0);
  });
});

describe('token inspection', () => {
  const b64 = (o: object) => btoa(JSON.stringify(o)).replace(/=+$/, '').replace(/\+/g, '-').replace(/\//g, '_');
  const now = 1_800_000_000_000;
  it('explains problems before any request', () => {
    expect(inspectToken('').valid).toBe(false);
    expect(inspectToken('abc').problem).toMatch(/three/);
    expect(inspectToken(`x.${b64({ exp: now / 1000 - 10 })}.y`, now).problem).toMatch(/expired/);
    expect(inspectToken(`x.${b64({ scope: 'lens', exp: now / 1000 + 600 })}.y`, now).problem).toMatch(/Lens token/);
    const ok = inspectToken(`Bearer x.${b64({ sub: 'u@acme', tid: 't', exp: now / 1000 + 600 })}.y`, now);
    expect(ok).toMatchObject({ valid: true, subject: 'u@acme', minutesLeft: 10 });
  });
});
