import { afterEach, describe, expect, it, vi } from 'vitest';
import { SysteraApiClient } from '../api';
import { ALL_CONSTRAINTS } from '../constraints';
import { XRAY_LAYERS, computeLocalDrillDown } from '../layers';
import type { XRayView } from '../../types/xray';

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('Systera Web Workspace — Live API & X-Ray Layering', () => {
  it('defines all eight canonical X-Ray layers and all eight counterfactual constraints', () => {
    expect(XRAY_LAYERS).toHaveLength(8);
    expect(XRAY_LAYERS.map((l) => l.id)).toEqual([
      'product_capability',
      'domain',
      'service',
      'api',
      'data',
      'events',
      'infrastructure',
      'security_reliability',
    ]);
    expect(ALL_CONSTRAINTS).toHaveLength(8);
  });

  it('calls live X-Ray, Drill-Down, Inspect, and Counterfactual endpoints with Bearer auth', async () => {
    const calls: Array<{ url: string; init?: RequestInit }> = [];
    vi.stubGlobal(
      'fetch',
      vi.fn(async (url: string, init?: RequestInit) => {
        calls.push({ url, init });
        return {
          ok: true,
          json: async () => ({ items: [] }),
        } as Response;
      }),
    );

    const client = new SysteraApiClient({ baseUrl: 'http://localhost:8080', token: 'jwt-123' });
    await client.listProjects();
    await client.getXRay('model-1', { version: '3', layers: ['service', 'data'] });
    await client.getDrillDown('model-1', 'node-1', { depth: 2, direction: 'out' });
    await client.inspectComponent('model-1', 'node-1', { version: '3' });
    await client.generateAlternatives('model-1', { scale: 5, cost: 1 }, 'Checkout');
    await client.diffAlternatives('model-1', 'alt-a', 'alt-b');

    expect(calls.map((c) => c.url)).toEqual([
      'http://localhost:8080/v1/projects',
      'http://localhost:8080/v1/architecture/model-1/xray?version=3&layers=service%2Cdata',
      'http://localhost:8080/v1/architecture/model-1/xray/nodes/node-1/drilldown?version=current&depth=2&direction=out',
      'http://localhost:8080/v1/architecture/model-1/xray/nodes/node-1/inspect?version=3',
      'http://localhost:8080/v1/architecture/model-1/alternatives',
      'http://localhost:8080/v1/architecture/model-1/alternatives/diff?from=alt-a&to=alt-b',
    ]);
    for (const c of calls) {
      expect((c.init?.headers as Record<string, string>).Authorization).toBe('Bearer jwt-123');
    }
  });

  it('computes directional neighbourhood drill-down on an XRayView snapshot', () => {
    const view: XRayView = {
      model_id: 'm1',
      version: 1,
      graph_hash: 'h1',
      total_nodes: 3,
      total_edges: 2,
      layers: [
        {
          id: 'domain',
          label: 'Domain',
          order: 2,
          node_count: 1,
          edge_count: 0,
          nodes: [{ id: 'd1', key: 'domain:orders', type: 'domain', name: 'Orders', layer: 'domain', evidence_state: 'PROPOSED' }],
          edges: [],
        },
        {
          id: 'service',
          label: 'Service',
          order: 3,
          node_count: 1,
          edge_count: 0,
          nodes: [{ id: 's1', key: 'svc:checkout', type: 'service', name: 'Checkout', layer: 'service', evidence_state: 'PROPOSED' }],
          edges: [],
        },
        {
          id: 'data',
          label: 'Data',
          order: 5,
          node_count: 1,
          edge_count: 0,
          nodes: [{ id: 'db1', key: 'db:orders', type: 'database', name: 'Orders DB', layer: 'data', evidence_state: 'PROPOSED' }],
          edges: [],
        },
      ],
      cross_layer_edges: [
        { id: 'e1', source_node_id: 'd1', target_node_id: 's1', relation_type: 'owns', source_layer: 'domain', target_layer: 'service', cross_layer: true },
        { id: 'e2', source_node_id: 's1', target_node_id: 'db1', relation_type: 'writes', source_layer: 'service', target_layer: 'data', cross_layer: true },
      ],
    };

    const dd = computeLocalDrillDown(view, 's1', { depth: 1, direction: 'out' });
    const ids = new Set(dd?.layers.flatMap((l) => l.nodes.map((n) => n.id)));
    expect(ids.has('s1')).toBe(true);
    expect(ids.has('db1')).toBe(true);
    expect(ids.has('d1')).toBe(false);
  });
});
