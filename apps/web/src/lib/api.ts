import type {
  AlternativeDiff,
  ArchitectureModel,
  ComponentInspection,
  ConstraintId,
  CounterfactualSet,
  Direction,
  GraphVersion,
  ProjectSummary,
  XRayDrillDown,
  XRayLayerId,
  XRayView,
} from '../types/xray';

export interface ApiClientConfig {
  baseUrl?: string;
  token?: string;
}

export class SysteraApiClient {
  private readonly baseUrl: string;
  private readonly token: string;

  constructor({ baseUrl = '', token = '' }: ApiClientConfig = {}) {
    this.baseUrl = baseUrl.replace(/\/$/, '');
    this.token = token;
  }

  private async request<T>(path: string, init?: RequestInit): Promise<T> {
    const headers: Record<string, string> = {
      Accept: 'application/json',
      ...(init?.body ? { 'Content-Type': 'application/json' } : {}),
      ...(this.token ? { Authorization: `Bearer ${this.token}` } : {}),
    };
    const res = await fetch(`${this.baseUrl}${path}`, { ...init, headers });
    if (!res.ok) {
      const text = await res.text();
      throw new Error(`HTTP ${res.status}: ${text.slice(0, 240)}`);
    }
    return (await res.json()) as T;
  }

  async listProjects(): Promise<ProjectSummary[]> {
    const res = await this.request<{ items?: ProjectSummary[] }>('/v1/projects');
    return res.items ?? [];
  }

  getModelForProject(projectId: string): Promise<ArchitectureModel> {
    return this.request<ArchitectureModel>(`/v1/projects/${encodeURIComponent(projectId)}/architecture`);
  }

  async listVersions(modelId: string): Promise<GraphVersion[]> {
    const res = await this.request<{ items?: GraphVersion[] }>(
      `/v1/architecture/${encodeURIComponent(modelId)}/versions`,
    );
    return res.items ?? [];
  }

  getXRay(modelId: string, opts: { version?: string; layers?: XRayLayerId[] } = {}): Promise<XRayView> {
    const qs = new URLSearchParams({ version: opts.version || 'current' });
    if (opts.layers && opts.layers.length > 0) qs.set('layers', opts.layers.join(','));
    return this.request<XRayView>(`/v1/architecture/${encodeURIComponent(modelId)}/xray?${qs}`);
  }

  getDrillDown(
    modelId: string,
    nodeId: string,
    opts: { version?: string; depth?: number; direction?: Direction; targetLayers?: XRayLayerId[] } = {},
  ): Promise<XRayDrillDown> {
    const qs = new URLSearchParams({
      version: opts.version || 'current',
      depth: String(opts.depth ?? 2),
      direction: opts.direction ?? 'both',
    });
    if (opts.targetLayers && opts.targetLayers.length > 0) qs.set('target_layers', opts.targetLayers.join(','));
    return this.request<XRayDrillDown>(
      `/v1/architecture/${encodeURIComponent(modelId)}/xray/nodes/${encodeURIComponent(nodeId)}/drilldown?${qs}`,
    );
  }

  inspectComponent(modelId: string, nodeId: string, opts: { version?: string } = {}): Promise<ComponentInspection> {
    const qs = new URLSearchParams({ version: opts.version || 'current' });
    return this.request<ComponentInspection>(
      `/v1/architecture/${encodeURIComponent(modelId)}/xray/nodes/${encodeURIComponent(nodeId)}/inspect?${qs}`,
    );
  }

  getAlternatives(modelId: string): Promise<CounterfactualSet> {
    return this.request<CounterfactualSet>(`/v1/architecture/${encodeURIComponent(modelId)}/alternatives`);
  }

  generateAlternatives(
    modelId: string,
    weights: Partial<Record<ConstraintId, number>>,
    subject?: string,
  ): Promise<CounterfactualSet> {
    return this.request<CounterfactualSet>(`/v1/architecture/${encodeURIComponent(modelId)}/alternatives`, {
      method: 'POST',
      body: JSON.stringify({ subject, constraints: { weights } }),
    });
  }

  diffAlternatives(modelId: string, fromAltId: string, toAltId: string): Promise<AlternativeDiff> {
    const qs = new URLSearchParams({ from: fromAltId, to: toAltId });
    return this.request<AlternativeDiff>(
      `/v1/architecture/${encodeURIComponent(modelId)}/alternatives/diff?${qs}`,
    );
  }
}
