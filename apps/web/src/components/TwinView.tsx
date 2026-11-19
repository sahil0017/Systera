import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import type { SysteraApiClient } from '../lib/api';
import { ago } from '../lib/research';
import {
  CHECK_LABEL, DIMENSIONS, FAILURE_KINDS, REPORT_LABEL, SOURCE_HINT, assumptionsToForm, blankForm, checkTone, formToAssumptions,
  reportTone, shortHash, visibleChecks, type AssumptionForm,
} from '../lib/twin';
import type { Dimension, Twin, TwinBuild, TwinCheck, TwinNodeRef, ValueSource } from '../types/twin';
import { Empty, ErrorNote, Skeleton, errorText } from './ui';

interface Props {
  api: SysteraApiClient;
  modelId: string;
  version: string;
  nodes: Array<{ id: string; name: string; type: string }>;
  onOpenNode: (nodeId: string) => void;
}

type View = 'report' | 'model';

/** Design-time Digital Twin: assumptions in, a normalized model and a six-dimension validation report out. */
export function TwinView({ api, modelId, version, nodes, onOpenNode }: Props) {
  const [form, setForm] = useState<AssumptionForm>(blankForm);
  const [edited, setEdited] = useState(0); // bumps on every form edit
  const [current, setCurrent] = useState<TwinBuild | null>(null);
  const [builds, setBuilds] = useState<TwinBuild[] | null>(null);
  const [busy, setBusy] = useState<'preview' | 'save' | 'load' | null>('load');
  const [error, setError] = useState<string | null>(null);
  const [flash, setFlash] = useState<string | null>(null);
  const [view, setView] = useState<View>('report');
  const [dim, setDim] = useState<Dimension | ''>('');
  const [problemsOnly, setProblemsOnly] = useState(true);
  const seq = useRef(0);

  const { assumptions, problems } = useMemo(() => formToAssumptions(form), [form]);

  const preview = useCallback(async (a = assumptions) => {
    const my = ++seq.current;
    setBusy('preview');
    setError(null);
    try {
      const r = await api.buildTwin(modelId, { version, assumptions: a, dryRun: true });
      if (my === seq.current) setCurrent(r.build);
    } catch (e) {
      if (my === seq.current) setError(errorText(e, 'this architecture'));
    } finally {
      if (my === seq.current) setBusy(null);
    }
  }, [api, assumptions, modelId, version]);

  // Open: show the latest saved build, or a preview with no assumptions.
  useEffect(() => {
    let live = true;
    setBusy('load');
    setCurrent(null);
    setError(null);
    setFlash(null);
    (async () => {
      try {
        const list = await api.listTwins(modelId);
        if (!live) return;
        setBuilds(list);
        if (list[0]?.id) {
          const full = await api.getTwin(list[0].id);
          if (!live) return;
          setForm(assumptionsToForm(full.assumptions));
          setCurrent(full);
          setBusy(null);
        } else {
          setForm(blankForm());
          const r = await api.buildTwin(modelId, { version, assumptions: formToAssumptions(blankForm()).assumptions, dryRun: true });
          if (!live) return;
          setCurrent(r.build);
          setBusy(null);
        }
      } catch (e) {
        if (live) { setError(errorText(e, 'this architecture')); setBusy(null); }
      }
    })();
    return () => { live = false; };
  }, [api, modelId, version]);

  // Re-validate shortly after the assumptions change.
  useEffect(() => {
    if (!edited || problems.length) return;
    const t = window.setTimeout(() => void preview(), 500);
    return () => window.clearTimeout(t);
  }, [edited]); // eslint-disable-line react-hooks/exhaustive-deps

  const edit = (patch: Partial<AssumptionForm>) => { setForm((f) => ({ ...f, ...patch })); setEdited((n) => n + 1); setFlash(null); };

  const saved = current && builds?.find((b) => b.assumptions_hash === current.assumptions_hash && b.graph_version === current.graph_version && b.builder_version === current.builder_version);
  const isPreview = !!current && !current.id;

  const save = async () => {
    if (problems.length) return;
    seq.current++;
    setBusy('save');
    setError(null);
    try {
      const r = await api.buildTwin(modelId, { version, assumptions });
      setCurrent(r.build);
      setFlash(r.created ? 'Build saved. It is reproducible from the graph version and assumptions shown.' : 'An identical build was already saved; opened it.');
      setBuilds(await api.listTwins(modelId));
    } catch (e) {
      setError(errorText(e, 'this architecture'));
    } finally {
      setBusy(null);
    }
  };

  const openBuild = async (b: TwinBuild) => {
    if (!b.id) return;
    seq.current++;
    setBusy('load');
    setError(null);
    setFlash(null);
    try {
      const full = await api.getTwin(b.id);
      setForm(assumptionsToForm(full.assumptions));
      setCurrent(full);
    } catch (e) {
      setError(errorText(e, 'this build'));
    } finally {
      setBusy(null);
    }
  };

  const report = current?.report;
  const twin = current?.twin;

  return (
    <main className="twin-view">
      <div className="view-head">
        <div>
          <h2>Digital Twin</h2>
          <p className="insp-sub">An executable model of this architecture plus the workload, environment and failure assumptions you state, validated across six dimensions before any simulation runs.</p>
        </div>
        <div className="view-head-actions">
          <div className="seg" role="tablist" aria-label="Twin view">
            <button type="button" role="tab" aria-selected={view === 'report'} className={view === 'report' ? 'on' : ''} onClick={() => setView('report')}>Validation</button>
            <button type="button" role="tab" aria-selected={view === 'model'} className={view === 'model' ? 'on' : ''} onClick={() => setView('model')}>Model</button>
          </div>
        </div>
      </div>

      <div className="twin-basis" role="note">
        <strong>Design intent, not production.</strong> {current?.twin?.disclaimer ?? 'Built from the declared architecture and stated assumptions; no telemetry is read and nothing here is a measurement.'}
      </div>

      <div className="twin-grid">
        <aside className="twin-side">
          <AssumptionsForm form={form} nodes={nodes} problems={problems} onEdit={edit} />
          <div className="twin-save">
            {isPreview && !saved && <span className="row-meta">{busy === 'preview' ? 'Validating…' : 'Unsaved preview'}</span>}
            {(saved || (current?.id && !isPreview)) && <span className="row-meta">Saved {(saved ?? current)?.created_at ? ago((saved ?? current)!.created_at!) : ''}</span>}
            <button type="button" className="btn btn-primary" disabled={!!problems.length || busy === 'save' || !!saved} onClick={() => void save()}>
              {busy === 'save' ? 'Saving…' : saved ? 'Saved' : 'Save build'}
            </button>
          </div>
          <BuildHistory builds={builds} currentId={current?.id ?? saved?.id} onOpen={(b) => void openBuild(b)} />
        </aside>

        <section className="twin-main" aria-busy={!!busy}>
          {flash && <div className="cf-banner">{flash}</div>}
          {error && <ErrorNote error={error} onRetry={() => void preview()} />}
          {!current && busy && <Skeleton rows={6} />}
          {current && report && twin && (
            <>
              <BuildHeader build={current} stale={busy === 'preview'} />
              {view === 'report' ? (
                <ReportView checks={report.checks} dims={report.dimensions} limitations={report.limitations} dim={dim} onDim={setDim}
                  problemsOnly={problemsOnly} onProblemsOnly={setProblemsOnly} onOpenNode={onOpenNode} />
              ) : (
                <ModelView twin={twin} onOpenNode={onOpenNode} />
              )}
            </>
          )}
        </section>
      </div>
    </main>
  );
}

function AssumptionsForm({ form, nodes, problems, onEdit }: {
  form: AssumptionForm; nodes: Props['nodes']; problems: string[]; onEdit: (p: Partial<AssumptionForm>) => void;
}) {
  const sortedNodes = useMemo(() => [...nodes].sort((a, b) => a.name.localeCompare(b.name)), [nodes]);
  const setFailure = (i: number, patch: Partial<AssumptionForm['failures'][number]>) =>
    onEdit({ failures: form.failures.map((f, j) => (j === i ? { ...f, ...patch } : f)) });
  return (
    <div className="surface write-form twin-form">
      <div className="section-label">Workload</div>
      <div className="form-grid">
        <label className="field"><span>Peak load (req/s)</span><input inputMode="decimal" value={form.peakRps} onChange={(e) => onEdit({ peakRps: e.target.value })} placeholder="not stated" /></label>
        <label className="field"><span>Reads (% of requests)</span><input inputMode="decimal" value={form.readRatio} onChange={(e) => onEdit({ readRatio: e.target.value })} placeholder="e.g. 90" /></label>
        <label className="field"><span>Avg payload (KB)</span><input inputMode="decimal" value={form.payloadKb} onChange={(e) => onEdit({ payloadKb: e.target.value })} /></label>
        <label className="field"><span>Growth (%/month)</span><input inputMode="decimal" value={form.growthPct} onChange={(e) => onEdit({ growthPct: e.target.value })} /></label>
      </div>
      <div className="section-label">Environment</div>
      <div className="form-grid">
        <label className="field"><span>Cloud</span><input value={form.cloud} onChange={(e) => onEdit({ cloud: e.target.value })} placeholder="e.g. AWS" /></label>
        <label className="field"><span>Regions</span><input value={form.regions} onChange={(e) => onEdit({ regions: e.target.value })} placeholder="comma separated" /></label>
        <label className="field"><span>Default replicas</span><input inputMode="numeric" value={form.defaultReplicas} onChange={(e) => onEdit({ defaultReplicas: e.target.value })} placeholder="1 if unstated" /></label>
        <label className="field"><span>Default timeout (ms)</span><input inputMode="numeric" value={form.defaultTimeoutMs} onChange={(e) => onEdit({ defaultTimeoutMs: e.target.value })} placeholder="none if unstated" /></label>
      </div>
      <p className="row-meta">Defaults apply only where the architecture states nothing; declared values on the graph always win.</p>

      <div className="row-between">
        <div className="section-label">Failure assumptions · {form.failures.length}</div>
        <button type="button" className="micro-btn" onClick={() => onEdit({ failures: [...form.failures, { kind: 'DEPENDENCY_OUTAGE', target: '', note: '' }] })}>+ Add</button>
      </div>
      {form.failures.length === 0 && <p className="row-meta">None. Failure simulation will inject what you list here.</p>}
      {form.failures.map((f, i) => {
        const meta = FAILURE_KINDS.find((k) => k.id === f.kind);
        return (
          <div key={i} className="twin-failure">
            <select value={f.kind} aria-label={`Failure ${i + 1} kind`} onChange={(e) => setFailure(i, { kind: e.target.value as typeof f.kind })}>
              {FAILURE_KINDS.map((k) => <option key={k.id} value={k.id}>{k.label}</option>)}
            </select>
            <select value={f.target} aria-label={`Failure ${i + 1} target`} onChange={(e) => setFailure(i, { target: e.target.value })}>
              <option value="">{meta?.needsTarget ? 'Pick a component…' : 'Whole system'}</option>
              {sortedNodes.map((n) => <option key={n.id} value={n.id}>{n.name} · {n.type}</option>)}
            </select>
            <button type="button" className="micro-btn" aria-label={`Remove failure ${i + 1}`} onClick={() => onEdit({ failures: form.failures.filter((_, j) => j !== i) })}>×</button>
            <input className="span-all" value={f.note} onChange={(e) => setFailure(i, { note: e.target.value })} placeholder="Note (optional)" aria-label={`Failure ${i + 1} note`} />
          </div>
        );
      })}
      <label className="field"><span>Notes</span>
        <textarea rows={2} value={form.notes} onChange={(e) => onEdit({ notes: e.target.value })} placeholder="Where these numbers come from" />
      </label>
      {problems.length > 0 && <div className="cf-error">{problems.join(' ')}</div>}
    </div>
  );
}

function BuildHistory({ builds, currentId, onOpen }: { builds: TwinBuild[] | null; currentId?: string; onOpen: (b: TwinBuild) => void }) {
  if (!builds) return null;
  return (
    <div className="twin-history">
      <div className="section-label">Saved builds · {builds.length}</div>
      {builds.length === 0 && <p className="row-meta">None yet. Save a build to make it reproducible and shareable.</p>}
      {builds.map((b) => (
        <button key={b.id} type="button" className={`pick ${b.id === currentId ? 'on' : ''}`} onClick={() => onOpen(b)}>
          <span className="row-title">
            <span className={`tone tone-${reportTone(b.status)}`}>{REPORT_LABEL[b.status]}</span> graph v{b.graph_version}
          </span>
          <span className="row-meta">{b.error_count} errors · {b.warning_count} warnings · {b.assumptions.workload?.peak_rps ? `${b.assumptions.workload.peak_rps} req/s` : 'no load stated'} · {b.created_at ? ago(b.created_at) : ''}</span>
        </button>
      ))}
    </div>
  );
}

function BuildHeader({ build, stale }: { build: TwinBuild; stale: boolean }) {
  return (
    <div className={`surface twin-head ${stale ? 'stale' : ''}`}>
      <div className={`twin-status tone-${reportTone(build.status)}`}>{REPORT_LABEL[build.status]}</div>
      <div className="twin-counts">
        <strong>{build.error_count}</strong> errors · <strong>{build.warning_count}</strong> warnings
      </div>
      <dl className="twin-repro" aria-label="Reproducibility">
        <div><dt>Graph</dt><dd>v{build.graph_version} <span className="mono">{shortHash(build.graph_hash)}</span></dd></div>
        <div><dt>Assumptions</dt><dd className="mono">{shortHash(build.assumptions_hash)}</dd></div>
        <div><dt>Builder</dt><dd className="mono">{build.builder_version}</dd></div>
      </dl>
    </div>
  );
}

function ReportView({ checks, dims, limitations, dim, onDim, problemsOnly, onProblemsOnly, onOpenNode }: {
  checks: TwinCheck[]; dims: NonNullable<TwinBuild['report']>['dimensions']; limitations: string[]; dim: Dimension | ''; onDim: (d: Dimension | '') => void;
  problemsOnly: boolean; onProblemsOnly: (v: boolean) => void; onOpenNode: (id: string) => void;
}) {
  const shown = visibleChecks(checks, dim, problemsOnly);
  return (
    <>
      <div className="twin-dims">
        {DIMENSIONS.map((d) => {
          const s = dims.find((x) => x.dimension === d.id);
          if (!s) return null;
          return (
            <button key={d.id} type="button" className={`twin-dim ${dim === d.id ? 'active' : ''}`} aria-pressed={dim === d.id} onClick={() => onDim(dim === d.id ? '' : d.id)}>
              <span className="row-between"><strong>{d.label}</strong><span className={`tone tone-${checkTone(s.status)}`}>{CHECK_LABEL[s.status]}</span></span>
              <span className="row-meta">{d.covers}</span>
              <span className="twin-dim-bar" aria-hidden="true">
                {s.failed > 0 && <i className="bad" style={{ flex: s.failed }} />}
                {s.warned > 0 && <i className="warn" style={{ flex: s.warned }} />}
                {s.passed > 0 && <i className="ok" style={{ flex: s.passed }} />}
                {s.not_applicable > 0 && <i className="na" style={{ flex: s.not_applicable }} />}
              </span>
              <span className="row-meta">{s.elements} element{s.elements === 1 ? '' : 's'} · {s.passed} pass{s.warned ? ` · ${s.warned} warn` : ''}{s.failed ? ` · ${s.failed} fail` : ''}{s.not_applicable ? ` · ${s.not_applicable} n/a` : ''}</span>
            </button>
          );
        })}
      </div>

      <div className="row-between twin-checks-head">
        <div className="section-label">{dim ? DIMENSIONS.find((d) => d.id === dim)?.label : 'All dimensions'} · {shown.length} checks</div>
        <label className="check"><input type="checkbox" checked={problemsOnly} onChange={(e) => onProblemsOnly(e.target.checked)} /> Only checks with findings</label>
      </div>
      {shown.length === 0 && <Empty>{problemsOnly ? 'No findings here. Untick the filter to see every check.' : 'No checks.'}</Empty>}
      <div className="rows">{shown.map((c) => <CheckCard key={c.id} c={c} onOpenNode={onOpenNode} />)}</div>

      <details className="twin-limits">
        <summary>Limitations of this build</summary>
        <ul className="insp-list">{limitations.map((l) => <li key={l}>{l}</li>)}</ul>
      </details>
    </>
  );
}

function CheckCard({ c, onOpenNode }: { c: TwinCheck; onOpenNode: (id: string) => void }) {
  const findings = c.findings ?? [];
  return (
    <div className={`surface twin-check ${checkTone(c.status)}`}>
      <div className="row-between">
        <span className="row-title"><span className={`tone tone-${checkTone(c.status)}`}>{CHECK_LABEL[c.status]}</span> {c.title}</span>
        <span className="row-meta">{DIMENSIONS.find((d) => d.id === c.dimension)?.label}</span>
      </div>
      {c.detail && <p className="row-meta">{c.detail}</p>}
      {findings.map((f, i) => (
        <div key={i} className={`twin-finding sev-${f.severity.toLowerCase()}`}>
          <span className={`tone tone-${f.severity === 'ERROR' ? 'bad' : f.severity === 'WARNING' ? 'warn' : 'muted'}`}>{f.severity.toLowerCase()}</span>
          <div>
            <p>{f.message}</p>
            {(f.subjects?.length ?? 0) > 0 && (
              <div className="chip-wrap">
                {f.subjects!.slice(0, 8).map((s) => <NodeChip key={s.id} n={s} onOpen={onOpenNode} />)}
                {f.subjects!.length > 8 && <span className="row-meta">+{f.subjects!.length - 8} more</span>}
              </div>
            )}
            {f.relies_on_defaults && <span className="row-meta twin-default-note">Decided by a builder default, not by your design. Declare the value to confirm.</span>}
          </div>
        </div>
      ))}
      {c.remediation && (c.status === 'FAIL' || c.status === 'WARN') && <p className="twin-fix"><strong>Fix:</strong> {c.remediation}</p>}
    </div>
  );
}

function NodeChip({ n, onOpen }: { n: TwinNodeRef; onOpen: (id: string) => void }) {
  return <button type="button" className="chip" title={`Open ${n.name} in X-Ray`} onClick={() => onOpen(n.id)}>{n.name} <span className="row-meta">· {n.type}</span></button>;
}

function Src({ s }: { s: ValueSource | string }) {
  const tone = s === 'DECLARED' ? 'ok' : s === 'ASSUMED' ? 'muted' : 'warn';
  return <span className={`tone tone-${tone} twin-src`} title={SOURCE_HINT[s as ValueSource] ?? s}>{s.toLowerCase()}</span>;
}

function ModelView({ twin, onOpenNode }: { twin: Twin; onOpenNode: (id: string) => void }) {
  const sloName = useMemo(() => new Map(twin.quality.slos.map((s) => [s.id, s.name])), [twin.quality.slos]);
  const sync = twin.behaviour.interactions.filter((i) => i.relation === 'calls');
  const q = twin.quality;
  return (
    <div className="twin-model">
      <section>
        <div className="section-label">Request paths · {twin.behaviour.request_paths.length}{twin.behaviour.paths_capped ? ' (capped)' : ''} · {q.coverage_pct}% under an SLO</div>
        {twin.behaviour.request_paths.length === 0 && <Empty>No request paths: add an actor or workload that calls an API or service.</Empty>}
        {twin.behaviour.request_paths.map((p, i) => (
          <div key={i} className="surface twin-path">
            <div className="twin-chain">
              {p.nodes.map((n, j) => (
                <span key={`${n.id}-${j}`} className="twin-chain-step">
                  {j > 0 && <span className="twin-arrow" aria-hidden="true">→</span>}
                  <NodeChip n={n} onOpen={onOpenNode} />
                </span>
              ))}
              <span className="tone tone-muted">ends: {p.ends_at.toLowerCase().replace('_', ' ')}</span>
            </div>
            <div className="row-meta">
              {p.sync_hops} sync hops · worst case {p.timeout_budget_ms ? `${p.timeout_budget_ms.toLocaleString()} ms` : 'unbounded (a hop has no timeout)'} ·
              {' '}retry amplification ×{p.retry_amplification} · {p.covered_by_slo_ids.length ? `SLO: ${p.covered_by_slo_ids.map((id) => sloName.get(id) ?? id).join(', ')}` : <span className="twin-warn-text">no SLO</span>}
            </div>
          </div>
        ))}
      </section>

      <section>
        <div className="section-label">Runtime · {twin.runtime.units.length} units · regions: {twin.runtime.regions.length ? twin.runtime.regions.join(', ') : 'unknown'} <Src s={twin.runtime.regions_source} /></div>
        <div className="twin-table-wrap">
          <table className="twin-table">
            <thead><tr><th>Unit</th><th>Replicas</th><th>Capacity / replica</th><th>Placed on</th></tr></thead>
            <tbody>
              {twin.runtime.units.map((u) => (
                <tr key={u.id}>
                  <td><button type="button" className="link-btn" onClick={() => onOpenNode(u.id)}>{u.name}</button> <span className="row-meta">{u.type}</span></td>
                  <td>{u.replicas} <Src s={u.replicas_source} /></td>
                  <td>{u.capacity_rps_per_replica ? `${u.capacity_rps_per_replica.toLocaleString()} req/s` : '—'} <Src s={u.capacity_source} /></td>
                  <td className="row-meta">{u.placed_on.map((p) => p.name).join(', ') || '—'}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </section>

      {sync.length > 0 && (
        <section>
          <div className="section-label">Calls · {sync.length}</div>
          <div className="twin-table-wrap">
            <table className="twin-table">
              <thead><tr><th>Call</th><th>Mode</th><th>Timeout</th><th>Retries</th><th>Protection</th></tr></thead>
              <tbody>
                {sync.map((c) => (
                  <tr key={c.edge_id}>
                    <td>{c.from.name} → {c.to.name} {c.protocol && <span className="row-meta">· {c.protocol}</span>}</td>
                    <td>{c.sync_mode.toLowerCase()} <Src s={c.sync_source} /></td>
                    <td>{c.timeout_ms ? `${c.timeout_ms} ms` : '—'} <Src s={c.timeout_source} /></td>
                    <td>{c.retries} <Src s={c.retries_source} /></td>
                    <td className="row-meta">{[c.backoff && 'backoff', c.circuit_breaker && 'breaker'].filter(Boolean).join(', ') || '—'}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </section>
      )}

      <div className="twin-model-cols">
        <section>
          <div className="section-label">Interfaces · {twin.interfaces.length}</div>
          {twin.interfaces.length === 0 && <p className="row-meta">None declared.</p>}
          {twin.interfaces.map((it) => (
            <div key={it.id} className="twin-mini">
              <button type="button" className="link-btn" onClick={() => onOpenNode(it.id)}>{it.name}</button>
              <span className="row-meta"> {it.kind.toLowerCase()} · {it.contract_basis.toLowerCase().replace(/_/g, ' ')} · {it.providers.length} provider{it.providers.length === 1 ? '' : 's'}, {it.consumers.length} consumer{it.consumers.length === 1 ? '' : 's'}</span>
            </div>
          ))}
        </section>
        <section>
          <div className="section-label">Data · {twin.data.length}</div>
          {twin.data.length === 0 && <p className="row-meta">No stores.</p>}
          {twin.data.map((d) => (
            <div key={d.id} className="twin-mini">
              <button type="button" className="link-btn" onClick={() => onOpenNode(d.id)}>{d.name}</button>
              <span className="row-meta"> {d.type} · {d.writers.length}w/{d.readers.length}r · {d.consistency || 'consistency ?'} · {d.replicates_to.length || d.fails_over_to.length ? 'redundant' : 'no replica'}</span>
            </div>
          ))}
        </section>
        <section>
          <div className="section-label">SLOs · {q.slos.length}</div>
          {q.slos.length === 0 && <p className="row-meta">None declared.</p>}
          {q.slos.map((s) => (
            <div key={s.id} className="twin-mini">
              <button type="button" className="link-btn" onClick={() => onOpenNode(s.id)}>{s.name}</button>
              <span className="row-meta"> {[s.availability_pct && `${s.availability_pct}%`, s.latency_p99_ms && `p99 ${s.latency_p99_ms} ms`, s.rto_minutes && `RTO ${s.rto_minutes} min`, s.rpo_minutes && `RPO ${s.rpo_minutes} min`].filter(Boolean).join(' · ') || 'no target'} · covers {s.covers.length}</span>
            </div>
          ))}
        </section>
      </div>

      <section>
        <div className="section-label">Filled in by the builder · {twin.defaults_applied.length}</div>
        {twin.defaults_applied.length === 0 ? <p className="row-meta">Nothing: every value came from the graph or your assumptions.</p> : (
          <ul className="insp-list">
            {twin.defaults_applied.map((d, i) => <li key={i}><strong>{d.subject.name}</strong> {d.field} = {String(d.value)} <Src s={d.source} /> <span className="row-meta">{d.reason}</span></li>)}
          </ul>
        )}
      </section>
    </div>
  );
}
