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
