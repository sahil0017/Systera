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
  DesignRun, EvidenceLink, Facet, KnowledgeDocument, KnowledgeIngest, KnowledgeResult, MemoryInput, KnowledgeUpdate, LearningOverview, MemoryRecord,
  Notification, OptionAssessment, OptionComparison, PulseSource, Report, RunReference, RunTask, Subscription, TopicDetail,
  TrendList, WebsiteComparison, WebsiteFinding, WebsiteFingerprint, WebsiteProfile,
} from '../types/research';
import type { TwinAssumptions, TwinBuild } from '../types/twin';

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
    return (await this.requestWithStatus<T>(path, init)).body;
  }

  private async requestWithStatus<T>(path: string, init?: RequestInit): Promise<{ status: number; body: T }> {
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
    return { status: res.status, body: (await res.json()) as T };
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

  // ---- Digital Twin (design-time) ----
  /** Builds and validates a twin. dryRun validates without recording; otherwise created is false when an identical build already existed. */
  async buildTwin(modelId: string, input: { version?: string; assumptions: TwinAssumptions; dryRun?: boolean }): Promise<{ build: TwinBuild; created: boolean }> {
    const r = await this.requestWithStatus<TwinBuild>(`/v1/architecture/${enc(modelId)}/twins`, {
      method: 'POST',
      body: JSON.stringify({ version: input.version || 'current', assumptions: input.assumptions, ...(input.dryRun ? { dry_run: true } : {}) }),
    });
    return { build: r.body, created: r.status === 201 };
  }

  async listTwins(modelId: string, limit = 20): Promise<TwinBuild[]> {
    const r = await this.request<{ items: TwinBuild[] | null }>(`/v1/architecture/${enc(modelId)}/twins?limit=${limit}`);
    return r.items ?? [];
  }

  getTwin(id: string): Promise<TwinBuild> {
    return this.request<TwinBuild>(`/v1/twins/${enc(id)}`);
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

  /** Ingest a document; `created` is false when identical content already existed (deduplicated). */
  async ingestKnowledge(input: KnowledgeIngest): Promise<{ document: KnowledgeDocument; created: boolean }> {
    const r = await this.requestWithStatus<KnowledgeDocument>('/v1/knowledge/documents', { method: 'POST', body: JSON.stringify(input) });
    return { document: r.body, created: r.status === 201 };
  }

  recordKnowledgeConflict(documentId: string, conflictingDocumentId: string, note: string): Promise<{ status: string }> {
    return this.request<{ status: string }>(`/v1/knowledge/documents/${enc(documentId)}/conflict`, {
      method: 'POST',
      body: JSON.stringify({ conflicting_document_id: conflictingDocumentId, note }),
    });
  }

  recordMemory(projectId: string, input: MemoryInput): Promise<MemoryRecord> {
    return this.request<MemoryRecord>(`/v1/projects/${enc(projectId)}/memory`, { method: 'POST', body: JSON.stringify(input) });
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
