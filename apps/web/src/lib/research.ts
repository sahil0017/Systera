import type {
  EvidenceClass, OptionAssessment, TrendScore, WebsiteFinding, WebsiteFingerprint, WebsiteProfile,
} from '../types/research';

export function ago(iso: string | undefined, now = Date.now()): string {
  if (!iso) return '—';
  const s = Math.max(0, (now - new Date(iso).getTime()) / 1000);
  if (s < 60) return 'just now';
  if (s < 3600) return `${Math.round(s / 60)} min ago`;
  if (s < 86400) return `${Math.round(s / 3600)} h ago`;
  return `${Math.round(s / 86400)} d ago`;
}

export function isStale(staleAfter: string | undefined, now = Date.now()): boolean {
  if (!staleAfter) return false;
  const t = new Date(staleAfter).getTime();
  return Number.isFinite(t) && t > 0 && t < now;
}

/** Badge class for a confidence band or report/knowledge status. */
export function toneOf(v: string | undefined): 'ok' | 'warn' | 'bad' | 'muted' {
  switch ((v ?? '').toUpperCase()) {
    case 'HIGH': case 'PUBLISHED': case 'VALIDATED': case 'ACTIVE': case 'APPROVED': case 'SUCCEEDED': case 'COMPLETED': case 'PROMOTED':
      return 'ok';
    case 'MEDIUM': case 'DRAFT': case 'RUNNING': case 'QUEUED': case 'PENDING': case 'LEASED': case 'PLANNED': case 'ANALYZED':
      return 'warn';
    case 'LOW': case 'REJECTED': case 'STALE': case 'FAILED': case 'CANCELLED': case 'CONFLICTED': case 'DISABLED': case 'SUSPENDED':
      return 'bad';
    default:
      return 'muted';
  }
}

/** Oldest-first score series for a sparkline. */
export function scoreSeries(scores: TrendScore[] | null | undefined): number[] {
  return (scores ?? [])
    .slice()
    .sort((a, b) => new Date(a.computed_at).getTime() - new Date(b.computed_at).getTime())
    .map((s) => s.score);
}

/** SVG polyline points for values scaled into a w×h box (0–100 domain). */
export function sparkPoints(values: number[], w = 120, h = 28): string {
  if (values.length === 0) return '';
  const y = (v: number) => round1(h - (Math.max(0, Math.min(100, v)) / 100) * h);
  if (values.length === 1) return `0,${y(values[0])} ${w},${y(values[0])}`;
  const step = w / (values.length - 1);
  return values.map((v, i) => `${round1(i * step)},${y(v)}`).join(' ');
}

/** Growth component: 50 is flat, above rising, below falling. */
export function growthLabel(g: number): { text: string; tone: 'ok' | 'warn' | 'muted' } {
  if (g >= 60) return { text: 'rising', tone: 'ok' };
  if (g <= 40) return { text: 'cooling', tone: 'warn' };
  return { text: 'steady', tone: 'muted' };
}

export function evidenceCounts(items: Array<{ evidence_class: EvidenceClass }>): Record<EvidenceClass, number> {
  const out: Record<EvidenceClass, number> = { OBSERVED: 0, INFERRED: 0, PROPOSED: 0 };
  for (const it of items) if (it.evidence_class in out) out[it.evidence_class] += 1;
  return out;
}

/** Findings grouped by kind, with groups in a fixed reading order. */
export function groupFindings(findings: WebsiteFinding[]): Array<{ kind: string; label: string; items: WebsiteFinding[] }> {
  const order = ['CAPABILITY', 'JOURNEY', 'DOMAIN', 'DATA_ENTITY', 'API', 'COMPONENT', 'SYSTEM_DESIGN', 'INFRASTRUCTURE', 'TECHNOLOGY_SIGNAL'];
  const by = new Map<string, WebsiteFinding[]>();
  for (const f of findings) by.set(f.kind, [...(by.get(f.kind) ?? []), f]);
  return [...by.entries()]
    .sort(([a], [b]) => rank(order, a) - rank(order, b) || a.localeCompare(b))
    .map(([kind, items]) => ({ kind, label: kindLabel(kind), items }));
}

const KIND_LABELS: Record<string, string> = {
  CAPABILITY: 'Capabilities', JOURNEY: 'Journeys', DOMAIN: 'Domains', DATA_ENTITY: 'Data entities', API: 'APIs',
  COMPONENT: 'Components', SYSTEM_DESIGN: 'System design', INFRASTRUCTURE: 'Infrastructure', TECHNOLOGY_SIGNAL: 'Technology signals',
};

export function kindLabel(kind: string): string {
  return KIND_LABELS[kind] ?? titleCase(kind);
}

/** Fingerprint layers that have elements, largest first. */
export function fingerprintLayers(fp: WebsiteFingerprint | null): Array<{ key: string; label: string; elements: NonNullable<WebsiteFingerprint['layers'][string]['elements']> }> {
  if (!fp) return [];
  return Object.entries(fp.layers ?? {})
    .map(([key, l]) => ({ key, label: titleCase(l.layer || key), elements: l.elements ?? [] }))
    .filter((l) => l.elements.length > 0)
    .sort((a, b) => b.elements.length - a.elements.length);
}

export function websiteSummary(w: WebsiteProfile): { capabilities: number; journeys: number; tech: number; pages: number } {
  return {
    capabilities: w.capabilities?.length ?? 0,
    journeys: w.journeys?.length ?? 0,
    tech: w.tech_signals?.length ?? 0,
    pages: w.source_observation_ids?.length ?? 0,
  };
}

/** Run is finished when it can no longer change. */
export function runFinished(state: string | undefined): boolean {
  return ['SUCCEEDED', 'COMPLETED', 'FAILED', 'CANCELLED'].includes((state ?? '').toUpperCase());
}

export const OPTION_CRITERIA = [
  { key: 'fit', label: 'Fit', hint: '1 poor … 5 excellent fit to requirements' },
  { key: 'security', label: 'Security', hint: '1 weak … 5 strong' },
  { key: 'operational_burden', label: 'Ops burden', hint: '1 light … 5 heavy' },
  { key: 'cost', label: 'Cost', hint: '1 cheap … 5 expensive' },
] as const;

export const MATURITIES = ['EXPERIMENTAL', 'EMERGING', 'MAINSTREAM', 'LEGACY', 'DEPRECATED'];

export function blankOption(name = ''): OptionAssessment {
  return { name, maturity: 'MAINSTREAM', fit: 3, security: 3, operational_burden: 3, cost: 3, rationale: {} };
}

export const RATIONALE_KEYS = ['fit', 'security', 'maturity', 'operational_burden', 'cost'] as const;

/** Problems the guardrail would reject, so the form can say so before sending. */
export function optionProblems(options: OptionAssessment[]): string[] {
  const out: string[] = [];
  if (options.length < 2) out.push('Add at least two options to compare.');
  const seen = new Set<string>();
  options.forEach((o, i) => {
    const name = o.name.trim() || `Option ${i + 1}`;
    if (!o.name.trim()) out.push(`${name} needs a name.`);
    else if (seen.has(name.toLowerCase())) out.push(`${name} is listed twice.`);
    seen.add(name.toLowerCase());
    const missing = RATIONALE_KEYS.filter((k) => !o.rationale[k]?.trim());
    if (missing.length) out.push(`${name}: add a rationale for ${missing.map((k) => k.replace('_', ' ')).join(', ')}.`);
  });
  return out;
}

export function titleCase(s: string): string {
  return s.replace(/[_-]+/g, ' ').toLowerCase().replace(/\b\w/g, (c) => c.toUpperCase());
}

export function shortId(id: string | undefined): string {
  return id ? id.slice(0, 8) : '';
}

function rank(order: string[], k: string) {
  const i = order.indexOf(k);
  return i < 0 ? order.length : i;
}

function round1(v: number) {
  return Math.round(v * 10) / 10;
}

// ---- Write-side forms (knowledge ingest, conflicts, project memory) ----

export const SOURCE_TYPES = ['PRIMARY_DOC', 'RFC', 'RELEASE_NOTES', 'ENGINEERING_BLOG', 'PAPER', 'COMMUNITY', 'INTERNAL'] as const;
export const MEMORY_TYPES = [
  { id: 'PROJECT', label: 'Project', hint: 'Lasting facts and decisions about this project' },
  { id: 'SEMANTIC', label: 'Semantic', hint: 'General lessons that apply beyond one run' },
  { id: 'EPISODIC', label: 'Episodic', hint: 'What happened in a specific run or session' },
  { id: 'WORKING', label: 'Working', hint: 'Short-lived context; give it an expiry' },
] as const;

/** Mirrors the backend limit (memory.MaxSummaryChars), which counts UTF-8 bytes. */
export const MAX_MEMORY_SUMMARY_BYTES = 4000;

const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
export const isUuid = (s: string) => UUID_RE.test(s.trim());

export function utf8Bytes(s: string): number {
  return new TextEncoder().encode(s).length;
}

/** The key the server derives when none is given: lower-cased title with spaces as dashes. */
export function defaultCanonicalKey(title: string): string {
  return title.trim().replace(/ /g, '-').toLowerCase();
}

export function ingestProblems(f: { title: string; raw_content: string; source_url?: string; published_at?: string }): string[] {
  const out: string[] = [];
  if (!f.title.trim()) out.push('Add a title.');
  if (!f.raw_content.trim()) out.push('Paste the document content.');
  if (f.source_url?.trim() && !/^https?:\/\/\S+$/i.test(f.source_url.trim())) out.push('Source URL must start with http:// or https://.');
  if (f.published_at && Number.isNaN(Date.parse(f.published_at))) out.push('Published date is not a valid date.');
  return out;
}

export function conflictProblems(docId: string, otherId: string, note: string): string[] {
  const out: string[] = [];
  if (!isUuid(otherId)) out.push('Choose the conflicting document (or paste its id).');
  else if (otherId.trim().toLowerCase() === docId.toLowerCase()) out.push('A document cannot conflict with itself.');
  if (!note.trim()) out.push('Explain what the two documents disagree on.');
  return out;
}

export function memoryProblems(f: { summary: string; run_id?: string; source_refs?: string; relevance?: number; memory_type: string; expires_at?: string }): string[] {
  const out: string[] = [];
  const s = f.summary.trim();
  if (!s) out.push('Write a short distilled summary.');
  else if (utf8Bytes(s) > MAX_MEMORY_SUMMARY_BYTES) out.push(`Summary is ${utf8Bytes(s)} bytes; memory stores distilled notes up to ${MAX_MEMORY_SUMMARY_BYTES}, never full transcripts.`);
  if (f.run_id?.trim() && !isUuid(f.run_id)) out.push('Run id must be a UUID.');
  if (f.relevance !== undefined && (f.relevance < 0 || f.relevance > 1)) out.push('Relevance must be between 0 and 1.');
  if (f.memory_type === 'WORKING' && !f.expires_at) out.push('Working memory should expire; pick an expiry.');
  if (f.expires_at && Date.parse(f.expires_at) <= Date.now()) out.push('Expiry must be in the future.');
  return out;
}

export function splitRefs(s: string): string[] {
  return [...new Set(s.split(/[\n,]+/).map((x) => x.trim()).filter(Boolean))];
}
