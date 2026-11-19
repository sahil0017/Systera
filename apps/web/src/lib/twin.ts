import type { CheckStatus, Dimension, FailureKind, ReportStatus, TwinAssumptions, TwinCheck, ValueSource } from '../types/twin';

export const DIMENSIONS: Array<{ id: Dimension; label: string; covers: string }> = [
  { id: 'STRUCTURE', label: 'Structure', covers: 'Components, ownership, dependencies' },
  { id: 'INTERFACES', label: 'Interfaces', covers: 'APIs, events, contracts' },
  { id: 'DATA', label: 'Data', covers: 'Stores, writers, consistency' },
  { id: 'RUNTIME', label: 'Runtime', covers: 'Replicas, regions, capacity' },
  { id: 'BEHAVIOUR', label: 'Behaviour', covers: 'Request paths, retries, timeouts' },
  { id: 'QUALITY_ATTRIBUTES', label: 'Quality attributes', covers: 'SLOs, RTO/RPO, coverage' },
];

export const FAILURE_KINDS: Array<{ id: FailureKind; label: string; needsTarget: boolean }> = [
  { id: 'NODE_LOSS', label: 'Lose a component', needsTarget: true },
  { id: 'DEPENDENCY_OUTAGE', label: 'Dependency outage', needsTarget: true },
  { id: 'LATENCY_SPIKE', label: 'Latency spike', needsTarget: false },
  { id: 'ZONE_LOSS', label: 'Lose a zone', needsTarget: false },
  { id: 'REGION_LOSS', label: 'Lose a region', needsTarget: false },
  { id: 'NETWORK_PARTITION', label: 'Network partition', needsTarget: false },
];

/** The editable form; numbers stay strings so empty fields mean "not stated". */
export interface AssumptionForm {
  peakRps: string;
  readRatio: string; // percent, 0–100
  payloadKb: string;
  growthPct: string;
  cloud: string;
  regions: string;
  defaultReplicas: string;
  defaultTimeoutMs: string;
  failures: Array<{ kind: FailureKind; target: string; note: string }>;
  notes: string;
}

export const blankForm = (): AssumptionForm => ({
  peakRps: '', readRatio: '', payloadKb: '', growthPct: '', cloud: '', regions: '', defaultReplicas: '', defaultTimeoutMs: '', failures: [], notes: '',
});

const numOrNull = (s: string): number | null => {
  const t = s.trim();
  if (!t) return null;
  const n = Number(t);
  return Number.isFinite(n) ? n : NaN;
};

/** Mirrors the control plane's NormalizeAssumptions so problems show before a request. */
export function formToAssumptions(f: AssumptionForm): { assumptions: TwinAssumptions; problems: string[] } {
  const problems: string[] = [];
  const peak = numOrNull(f.peakRps);
  const read = numOrNull(f.readRatio);
  const payload = numOrNull(f.payloadKb);
  const growth = numOrNull(f.growthPct);
  const replicas = numOrNull(f.defaultReplicas);
  const timeout = numOrNull(f.defaultTimeoutMs);
  if (peak !== null && (Number.isNaN(peak) || peak < 0)) problems.push('Peak load must be a positive number.');
  if (read !== null && (Number.isNaN(read) || read < 0 || read > 100)) problems.push('Read share must be between 0 and 100%.');
  if (payload !== null && (Number.isNaN(payload) || payload < 0)) problems.push('Payload size must be a positive number.');
  if (growth !== null && (Number.isNaN(growth) || growth < -100 || growth > 1000)) problems.push('Growth must be between −100 and 1000% a month.');
  if (replicas !== null && (Number.isNaN(replicas) || replicas < 1 || replicas > 1000 || !Number.isInteger(replicas))) problems.push('Default replicas must be a whole number from 1 to 1000.');
  if (timeout !== null && (Number.isNaN(timeout) || timeout < 1 || timeout > 600000 || !Number.isInteger(timeout))) problems.push('Default timeout must be a whole number of milliseconds (1–600000).');
  const regions = [...new Set(f.regions.split(',').map((r) => r.trim()).filter(Boolean))].sort();
  if (regions.length > 20) problems.push('At most 20 regions.');
  f.failures.forEach((x, i) => {
    const meta = FAILURE_KINDS.find((k) => k.id === x.kind);
    if (meta?.needsTarget && !x.target) problems.push(`Failure ${i + 1} (${meta.label.toLowerCase()}) needs a target component.`);
    if (x.note.length > 500) problems.push(`Failure ${i + 1}: note is longer than 500 characters.`);
  });
  if (f.failures.length > 50) problems.push('At most 50 failure assumptions.');
  if (f.notes.trim().length > 2000) problems.push('Notes must be at most 2000 characters.');

  const workload: TwinAssumptions['workload'] = { peak_rps: peak && !Number.isNaN(peak) ? peak : 0 };
  if (read !== null && !Number.isNaN(read)) workload.read_ratio = Math.round(read * 100) / 10000;
  if (payload) workload.avg_payload_kb = payload;
  if (growth) workload.growth_pct_per_month = growth;
  const environment: TwinAssumptions['environment'] = { regions };
  if (f.cloud.trim()) environment.cloud = f.cloud.trim();
  if (replicas && !Number.isNaN(replicas)) environment.default_replicas = replicas;
  if (timeout && !Number.isNaN(timeout)) environment.default_timeout_ms = timeout;
  const failures = f.failures.map((x) => ({ kind: x.kind, ...(x.target ? { target_node_id: x.target } : {}), ...(x.note.trim() ? { note: x.note.trim() } : {}) }));
  return { assumptions: { workload, environment, failures, ...(f.notes.trim() ? { notes: f.notes.trim() } : {}) }, problems };
}

export function assumptionsToForm(a: TwinAssumptions): AssumptionForm {
  const s = (n?: number) => (n ? String(n) : '');
  return {
    peakRps: s(a.workload?.peak_rps),
    readRatio: a.workload?.read_ratio !== undefined && a.workload.read_ratio !== null ? String(Math.round(a.workload.read_ratio * 10000) / 100) : '',
    payloadKb: s(a.workload?.avg_payload_kb),
    growthPct: s(a.workload?.growth_pct_per_month),
    cloud: a.environment?.cloud ?? '',
    regions: (a.environment?.regions ?? []).join(', '),
    defaultReplicas: s(a.environment?.default_replicas),
    defaultTimeoutMs: s(a.environment?.default_timeout_ms),
    failures: (a.failures ?? []).map((x) => ({ kind: x.kind, target: x.target_node_id ?? '', note: x.note ?? '' })),
    notes: a.notes ?? '',
  };
}

export const REPORT_LABEL: Record<ReportStatus, string> = { VALID: 'Valid', VALID_WITH_WARNINGS: 'Valid with warnings', INVALID: 'Invalid' };
export const reportTone = (s: ReportStatus) => (s === 'INVALID' ? 'bad' : s === 'VALID_WITH_WARNINGS' ? 'warn' : 'ok');
export const checkTone = (s: CheckStatus) => (s === 'FAIL' ? 'bad' : s === 'WARN' ? 'warn' : s === 'PASS' ? 'ok' : 'muted');
export const CHECK_LABEL: Record<CheckStatus, string> = { PASS: 'pass', WARN: 'warn', FAIL: 'fail', NOT_APPLICABLE: 'n/a' };

export const SOURCE_HINT: Record<ValueSource, string> = {
  DECLARED: 'Stated on the architecture graph',
  ASSUMED: 'From this build’s assumptions',
  DEFAULT: 'Filled in by the builder; declare it to make results meaningful',
  MISSING: 'Unknown',
};

/** Checks that need attention first (fail, warn), then the rest in report order. */
export function sortChecks(checks: TwinCheck[]): TwinCheck[] {
  const rank: Record<CheckStatus, number> = { FAIL: 0, WARN: 1, PASS: 2, NOT_APPLICABLE: 3 };
  return checks.map((c, i) => ({ c, i })).sort((a, b) => rank[a.c.status] - rank[b.c.status] || a.i - b.i).map((x) => x.c);
}

/** Filter checks by dimension and whether only problems are shown. */
export function visibleChecks(checks: TwinCheck[], dim: Dimension | '', problemsOnly: boolean): TwinCheck[] {
  return sortChecks(checks.filter((c) => (!dim || c.dimension === dim) && (!problemsOnly || c.status === 'FAIL' || c.status === 'WARN' || (c.findings ?? []).length > 0)));
}

export const shortHash = (h?: string) => (h ? h.slice(0, 10) : '—');
