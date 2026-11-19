import type {
  AlternativeDiff,
  ArchitectureModel,
  ComponentInspection,
  ConstraintId,
  CounterfactualSet,
  Direction,
  GraphVersion,
  LearningChallenge,
  Lesson,
  ProjectSummary,
  XRayDrillDown,
  XRayLayerId,
  XRayView,
} from '../types/xray';
import type {
  DesignRun, EvidenceLink, Facet, KnowledgeDocument, KnowledgeResult, KnowledgeUpdate, LearningOverview, MemoryRecord,
  Notification, OptionAssessment, OptionComparison, PulseSource, Report, RunReference, RunTask, Subscription, TopicDetail,
  TrendList, WebsiteComparison, WebsiteFinding, WebsiteFingerprint, WebsiteProfile,
} from '../types/research';

const enc = encodeURIComponent;

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

  /** Unauthenticated readiness probe, used to show whether the server is reachable. */
  async ready(): Promise<'ready' | 'not_ready' | 'unreachable'> {
    try {
      const res = await fetch(`${this.baseUrl}/readyz`, { headers: { Accept: 'application/json' } });
      return res.ok ? 'ready' : 'not_ready';
    } catch {
      return 'unreachable';
    }
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

  getLesson(modelId: string, nodeId: string, opts: { version?: string } = {}): Promise<Lesson> {
    const qs = new URLSearchParams({ version: opts.version || 'current' });
    return this.request<Lesson>(
      `/v1/architecture/${encodeURIComponent(modelId)}/learning/nodes/${encodeURIComponent(nodeId)}?${qs}`,
    );
  }

  challengeLesson(modelId: string, nodeId: string, challenge: string, opts: { version?: string } = {}): Promise<LearningChallenge> {
    return this.request<LearningChallenge>(
      `/v1/architecture/${encodeURIComponent(modelId)}/learning/nodes/${encodeURIComponent(nodeId)}/challenges`,
      { method: 'POST', body: JSON.stringify({ challenge, version: opts.version || 'current' }) },
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

  // ---- Learning overview ----
  getLearningOverview(modelId: string, opts: { version?: string } = {}): Promise<LearningOverview> {
    const qs = new URLSearchParams({ version: opts.version || 'current' });
    return this.request<LearningOverview>(`/v1/architecture/${enc(modelId)}/learning?${qs}`);
  }

  // ---- Engineering Pulse ----
  listTrends(opts: { facet?: Facet | ''; minScore?: number; limit?: number } = {}): Promise<TrendList> {
    const qs = new URLSearchParams({ limit: String(opts.limit ?? 50) });
    if (opts.facet) qs.set('facet', opts.facet);
    if (opts.minScore) qs.set('min_score', String(opts.minScore));
    return this.request<TrendList>(`/v1/trends?${qs}`);
  }

  getTopic(key: string): Promise<TopicDetail> {
    return this.request<TopicDetail>(`/v1/trends/topics/${enc(key)}`);
  }

  async listReports(opts: { topic?: string; status?: string } = {}): Promise<Report[]> {
    const qs = new URLSearchParams({ limit: '50' });
    if (opts.topic) qs.set('topic', opts.topic);
    if (opts.status) qs.set('status', opts.status);
    return (await this.request<{ items?: Report[] }>(`/v1/trends/reports?${qs}`)).items ?? [];
  }

  getReport(id: string): Promise<Report> {
    return this.request<Report>(`/v1/trends/reports/${enc(id)}`);
  }

  async listSources(): Promise<PulseSource[]> {
    return (await this.request<{ items?: PulseSource[] }>('/v1/trends/sources')).items ?? [];
  }

  async listKnowledgeUpdates(): Promise<KnowledgeUpdate[]> {
    return (await this.request<{ items?: KnowledgeUpdate[] }>('/v1/knowledge/updates')).items ?? [];
  }

  async listNotifications(agentType?: string): Promise<Notification[]> {
    const qs = new URLSearchParams({ limit: '50' });
    if (agentType) qs.set('agent_type', agentType);
    return (await this.request<{ items?: Notification[] }>(`/v1/knowledge/notifications?${qs}`)).items ?? [];
  }

  async listSubscriptions(): Promise<Subscription[]> {
    return (await this.request<{ items?: Subscription[] }>('/v1/knowledge/subscriptions')).items ?? [];
  }

  compareOptions(options: OptionAssessment[]): Promise<OptionComparison> {
    return this.request<OptionComparison>('/v1/trends/compare', { method: 'POST', body: JSON.stringify({ options }) });
  }

  // ---- Knowledge and memory ----
  async searchKnowledge(q: { query: string; include_stale?: boolean; primary_only?: boolean; limit?: number }): Promise<KnowledgeResult[]> {
    return (await this.request<{ results?: KnowledgeResult[] }>('/v1/knowledge/search', { method: 'POST', body: JSON.stringify(q) })).results ?? [];
  }

  getKnowledgeDocument(id: string): Promise<KnowledgeDocument> {
    return this.request<KnowledgeDocument>(`/v1/knowledge/documents/${enc(id)}`);
  }

  async projectMemory(projectId: string, query = ''): Promise<MemoryRecord[]> {
    const qs = new URLSearchParams(query ? { q: query } : {});
    return (await this.request<{ items?: MemoryRecord[] }>(`/v1/projects/${enc(projectId)}/memory?${qs}`)).items ?? [];
  }

  // ---- Runs ----
  getRun(id: string): Promise<DesignRun> {
    return this.request<DesignRun>(`/v1/runs/${enc(id)}`);
  }

  async listRunTasks(id: string): Promise<RunTask[]> {
    return (await this.request<{ items?: RunTask[] }>(`/v1/runs/${enc(id)}/tasks`)).items ?? [];
  }

  async runKnowledge(id: string): Promise<RunReference[]> {
    return (await this.request<{ items?: RunReference[] }>(`/v1/runs/${enc(id)}/knowledge`)).items ?? [];
  }

  // ---- Website investigations ----
  async listWebsites(): Promise<WebsiteProfile[]> {
    return (await this.request<{ items?: WebsiteProfile[] }>('/v1/websites')).items ?? [];
  }

  getWebsite(id: string): Promise<WebsiteProfile> {
    return this.request<WebsiteProfile>(`/v1/websites/${enc(id)}`);
  }

  async websiteFindings(id: string, kind: 'capabilities' | 'hypotheses'): Promise<WebsiteFinding[]> {
    return (await this.request<{ items?: WebsiteFinding[] }>(`/v1/websites/${enc(id)}/${kind}`)).items ?? [];
  }

  async websiteEvidence(id: string): Promise<EvidenceLink[]> {
    return (await this.request<{ items?: EvidenceLink[] }>(`/v1/websites/${enc(id)}/evidence`)).items ?? [];
  }

  getWebsiteFingerprint(id: string): Promise<WebsiteFingerprint> {
    return this.request<WebsiteFingerprint>(`/v1/websites/${enc(id)}/fingerprint`);
  }

  buildWebsiteFingerprint(id: string): Promise<WebsiteFingerprint> {
    return this.request<WebsiteFingerprint>(`/v1/websites/${enc(id)}/fingerprint`, { method: 'POST', body: '{}' });
  }

  compareWebsites(ids: string[]): Promise<WebsiteComparison> {
    return this.request<WebsiteComparison>('/v1/websites/compare', { method: 'POST', body: JSON.stringify({ website_ids: ids }) });
  }
}
