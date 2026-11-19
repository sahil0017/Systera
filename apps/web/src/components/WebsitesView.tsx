import { useEffect, useMemo, useState } from 'react';
import type { SysteraApiClient } from '../lib/api';
import { ago, evidenceCounts, fingerprintLayers, groupFindings, runFinished, titleCase, websiteSummary } from '../lib/research';
import type { WebsiteComparison, WebsiteFinding, WebsiteFingerprint, WebsiteProfile } from '../types/research';
import { RefCard } from './ResearchView';
import { Empty, ErrorNote, EvBadge, Skeleton, Tone, errorText, useAsync, type Async } from './ui';

interface Props {
  api: SysteraApiClient;
  selectedId: string;
  onSelect: (id: string) => void;
  onOpenModel: (modelId: string, name: string) => void;
  onOpenRun: (runId: string) => void;
}

type DetailTab = 'findings' | 'evidence' | 'fingerprint' | 'alternatives' | 'knowledge';

export function WebsitesView({ api, selectedId, onSelect, onOpenModel, onOpenRun }: Props) {
  const list = useAsync(() => api.listWebsites(), 'websites', 'website investigations');
  const [q, setQ] = useState('');
  const [picked, setPicked] = useState<string[]>([]);
  const [comparison, setComparison] = useState<WebsiteComparison | null>(null);
  const [comparing, setComparing] = useState(false);
  const [cmpError, setCmpError] = useState<string | null>(null);

  const sites = (list.data ?? []).filter((w) => !q || w.domain.toLowerCase().includes(q.toLowerCase()));
  useEffect(() => {
    if (!selectedId && !comparison && list.data && list.data.length > 0) onSelect(list.data[0].id);
  }, [comparison, list.data, onSelect, selectedId]);

  const toggle = (id: string) => setPicked((p) => (p.includes(id) ? p.filter((x) => x !== id) : p.length >= 5 ? p : [...p, id]));
  const compare = async () => {
    setComparing(true);
    setCmpError(null);
    try {
      setComparison(await api.compareWebsites(picked));
    } catch (e) {
      setCmpError(errorText(e, 'the comparison'));
    } finally {
      setComparing(false);
    }
  };

  return (
    <main className="sites">
      <aside className="sites-list">
        <div className="row-between list-head">
          <strong>Investigations{list.data ? ` (${list.data.length})` : ''}</strong>
          <button type="button" className="micro-btn" onClick={list.reload} title="Refresh">↻</button>
        </div>
        {(list.data?.length ?? 0) > 5 && <input className="mini-search full" placeholder="Filter by domain…" value={q} onChange={(e) => setQ(e.target.value)} />}
        {list.error && <ErrorNote error={list.error} onRetry={list.reload} />}
        {list.loading && !list.data && <Skeleton rows={5} />}
        {list.data?.length === 0 && <Empty>No investigations yet. Use the Engineering Lens extension on a website and choose “Send to Systera”.</Empty>}
        {sites.map((w) => {
          const s = websiteSummary(w);
          return (
            <div key={w.id} className={`site-row ${selectedId === w.id && !comparison ? 'active' : ''}`}>
              <input type="checkbox" checked={picked.includes(w.id)} onChange={() => toggle(w.id)} aria-label={`Select ${w.domain} for comparison`} />
              <button type="button" className="site-btn" onClick={() => { setComparison(null); onSelect(w.id); }}>
                <span className="row-title">{w.domain}</span>
                <span className="row-meta">{s.capabilities} capabilities · {s.pages} pages · {ago(w.created_at)}</span>
              </button>
              <Tone value={w.status} />
            </div>
          );
        })}
        {picked.length > 0 && (
          <div className="compare-dock">
            <span>{picked.length} selected{picked.length >= 5 ? ' (max)' : ''}</span>
            <button type="button" className="link-btn" onClick={() => setPicked([])}>Clear</button>
            <button type="button" className="btn btn-primary" disabled={picked.length < 2 || comparing} onClick={() => void compare()}>
              {comparing ? 'Comparing…' : picked.length < 2 ? 'Pick 2+' : `Compare ${picked.length}`}
            </button>
          </div>
        )}
        {cmpError && <ErrorNote error={cmpError} />}
      </aside>

      <section className="sites-main">
        {comparison ? (
          <ComparisonView c={comparison} onClose={() => setComparison(null)} onSelect={(id) => { setComparison(null); onSelect(id); }} />
        ) : selectedId ? (
          <WebsiteDetail key={selectedId} api={api} id={selectedId} onOpenModel={onOpenModel} onOpenRun={onOpenRun} />
        ) : list.data?.length === 0 ? null : <Skeleton rows={6} />}
      </section>
    </main>
  );
}

function WebsiteDetail({ api, id, onOpenModel, onOpenRun }: { api: SysteraApiClient; id: string } & Pick<Props, 'onOpenModel' | 'onOpenRun'>) {
  const [tab, setTab] = useState<DetailTab>('findings');
  const site = useAsync(() => api.getWebsite(id), `w:${id}`, 'this investigation');
  const caps = useAsync(() => api.websiteFindings(id, 'capabilities'), `wc:${id}`, 'capabilities');
  const hyps = useAsync(() => api.websiteFindings(id, 'hypotheses'), `wh:${id}`, 'hypotheses');
  const w = site.data;

  // Agent progress: poll the run until it can no longer change.
  const [tick, setTick] = useState(0);
  const run = useAsync(w?.run_id ? () => api.getRun(w.run_id!) : null, `r:${w?.run_id}:${tick}`, 'the agent run');
  const tasks = useAsync(w?.run_id ? () => api.listRunTasks(w.run_id!) : null, `t:${w?.run_id}:${tick}`, 'agent tasks');
  const done = runFinished(run.data?.state);
  useEffect(() => {
    if (!w?.run_id || done || run.error) return;
    const t = window.setTimeout(() => { setTick((n) => n + 1); caps.reload(); hyps.reload(); }, 5000);
    return () => window.clearTimeout(t);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [w?.run_id, done, run.error, tick]);

  if (site.error) return <ErrorNote error={site.error} onRetry={site.reload} />;
  if (!w) return <Skeleton rows={8} />;

  const findings = [...(caps.data ?? []), ...(hyps.data ?? [])];
  const counts = evidenceCounts(findings.length ? findings : (w.capabilities ?? []));
  const s = websiteSummary(w);
  const taskList = tasks.data ?? [];
  const finished = taskList.filter((t) => runFinished(t.state)).length;

  return (
    <div className="site-detail">
      <div className="view-head">
        <div>
          <div className="node-type">Website investigation</div>
          <h2>{w.domain}</h2>
          <div className="row-meta">{s.pages} pages observed · {s.capabilities} capabilities · {s.journeys} journeys · {s.tech} technology signals · {ago(w.created_at)}</div>
        </div>
        <div className="view-head-actions">
          <Tone value={w.status} />
          {w.model_id && <button type="button" className="btn btn-primary" onClick={() => onOpenModel(w.model_id!, w.domain)}>Open in X-Ray</button>}
        </div>
      </div>

      <div className="stat-row">
        <div className="stat"><span className="stat-n ev-text-OBSERVED">{counts.OBSERVED}</span><span>observed</span></div>
        <div className="stat"><span className="stat-n ev-text-INFERRED">{counts.INFERRED}</span><span>inferred</span></div>
        <div className="stat"><span className="stat-n ev-text-PROPOSED">{counts.PROPOSED}</span><span>proposed</span></div>
        {w.run_id && (
          <div className="stat run-stat">
            <span className="row-between"><strong>Agents</strong><Tone value={run.data?.state ?? 'PENDING'} /></span>
            <span className="meter-track"><span className="meter-fill" style={{ width: `${taskList.length ? (finished / taskList.length) * 100 : 0}%` }} /></span>
            <span className="row-meta">{finished}/{taskList.length} tasks done{!done && ' · updating'} · <button type="button" className="link-btn" onClick={() => onOpenRun(w.run_id!)}>provenance</button></span>
          </div>
        )}
      </div>

      {taskList.length > 0 && !done && (
        <div className="task-chips">
          {taskList.map((t) => <span key={t.id} className={`chip static tone-${runFinished(t.state) ? 'ok' : 'muted'}`} title={t.last_error || t.objective}>{titleCase(t.agent_type)} · {t.state.toLowerCase()}</span>)}
        </div>
      )}

      <div className="insp-tabs" role="tablist">
        {(['findings', 'evidence', 'fingerprint', 'alternatives', 'knowledge'] as DetailTab[]).map((t) => (
          <button key={t} type="button" role="tab" aria-selected={tab === t} className={`insp-tab ${tab === t ? 'active' : ''}`} onClick={() => setTab(t)}>
            {t === 'knowledge' ? 'Knowledge used' : titleCase(t)}
          </button>
        ))}
      </div>

      {tab === 'findings' && <Findings w={w} caps={caps} hyps={hyps} />}
      {tab === 'evidence' && <Evidence api={api} id={id} />}
      {tab === 'fingerprint' && <FingerprintTab api={api} id={id} />}
      {tab === 'alternatives' && <Alternatives w={w} />}
      {tab === 'knowledge' && <KnowledgeUsed api={api} runId={w.run_id} />}
    </div>
  );
}

function Findings({ w, caps, hyps }: { w: WebsiteProfile; caps: Async<WebsiteFinding[]>; hyps: Async<WebsiteFinding[]> }) {
  const [ev, setEv] = useState<'ALL' | 'OBSERVED' | 'INFERRED' | 'PROPOSED'>('ALL');
  const all = [...(caps.data ?? []), ...(hyps.data ?? [])];
  const groups = groupFindings(all.filter((f) => ev === 'ALL' || f.evidence_class === ev));
  if (caps.loading && !caps.data) return <Skeleton rows={6} />;
  if (caps.error) return <ErrorNote error={caps.error} onRetry={caps.reload} />;
  if (all.length === 0) {
    // Older investigations only carry the baseline profile.
    return (
      <div className="tab-body">
        {(w.capabilities ?? []).map((c) => (
          <div key={c.name} className="finding-card">
            <div className="row-between"><strong>{c.name}</strong><EvBadge value={c.evidence_class} /></div>
            <div className="insp-sub">{c.rationale}</div>
          </div>
        ))}
        {(w.capabilities ?? []).length === 0 && <Empty>No findings yet.</Empty>}
      </div>
    );
  }
  return (
    <div className="tab-body">
      <div className="seg">
        {(['ALL', 'OBSERVED', 'INFERRED', 'PROPOSED'] as const).map((e) => <button key={e} type="button" className={ev === e ? 'on' : ''} onClick={() => setEv(e)}>{e === 'ALL' ? 'All' : e.toLowerCase()}</button>)}
      </div>
      {groups.map((g) => (
        <div key={g.kind} className="insp-section">
          <h4>{g.label} · {g.items.length}</h4>
          {g.items.map((f) => (
            <details key={f.id} className="finding-card">
              <summary>
                <span className="grow"><strong>{f.name}</strong></span>
                {f.source === 'AGENT' && f.agent_type && <span className="chip static">{titleCase(f.agent_type)}</span>}
                <EvBadge value={f.evidence_class} />
              </summary>
              <p>{f.statement}</p>
              <div className="insp-sub">{f.rationale} · confidence {f.confidence.toLowerCase()} · {f.observation_ids?.length ?? 0} observations</div>
            </details>
          ))}
        </div>
      ))}
      {hyps.error && <ErrorNote error={hyps.error} onRetry={hyps.reload} />}
    </div>
  );
}

function Evidence({ api, id }: { api: SysteraApiClient; id: string }) {
  const res = useAsync(() => api.websiteEvidence(id), `we:${id}`, 'evidence links');
  const byRelation = useMemo(() => {
    const m = new Map<string, number>();
    for (const e of res.data ?? []) m.set(e.relation, (m.get(e.relation) ?? 0) + 1);
    return [...m.entries()];
  }, [res.data]);
  if (res.error) return <ErrorNote error={res.error} onRetry={res.reload} />;
  if (!res.data) return <Skeleton rows={6} />;
  if (res.data.length === 0) return <Empty>No evidence links recorded.</Empty>;
  return (
    <div className="tab-body">
      <div className="insp-sub">Every claim links back to what was observed on the page. {byRelation.map(([r, n]) => `${n} ${r.toLowerCase().replace(/_/g, ' ')}`).join(' · ')}</div>
      <div className="cf-table-wrap">
        <table className="cf-table">
          <thead><tr><th>Claim</th><th>Class</th><th>Relation</th><th>Finding</th><th>Observation</th></tr></thead>
          <tbody>
            {res.data.map((e) => (
              <tr key={e.id}>
                <td>{e.statement}<div className="row-meta">confidence {e.confidence.toLowerCase()}</div></td>
                <td><EvBadge value={e.claim_class} /></td>
                <td>{e.relation.toLowerCase().replace(/_/g, ' ')}</td>
                <td className="mono">{e.finding_type.toLowerCase()}<div className="row-meta">{e.finding_id.slice(0, 8)}</div></td>
                <td className="mono">{e.observation_id ? e.observation_id.slice(0, 8) : '—'}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}

function FingerprintTab({ api, id }: { api: SysteraApiClient; id: string }) {
  const res = useAsync(() => api.getWebsiteFingerprint(id), `fp:${id}`, 'a fingerprint');
  const [built, setBuilt] = useState<WebsiteFingerprint | null>(null);
  const [building, setBuilding] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const fp = built ?? res.data;
  const layers = fingerprintLayers(fp);

  const build = async () => {
    setBuilding(true);
    setErr(null);
    try { setBuilt(await api.buildWebsiteFingerprint(id)); } catch (e) { setErr(errorText(e, 'the fingerprint')); } finally { setBuilding(false); }
  };

  if (res.loading && !fp) return <Skeleton rows={6} />;
  if (!fp) {
    return (
      <div className="tab-body">
        <Empty>No architecture fingerprint has been built for this site yet.</Empty>
        <button type="button" className="btn btn-primary" disabled={building} onClick={() => void build()}>{building ? 'Building…' : 'Build fingerprint'}</button>
        {err && <ErrorNote error={err} />}
      </div>
    );
  }
  return (
    <div className="tab-body">
      <div className="cf-banner">{fp.disclaimer}</div>
      <div className="fp-grid">
        {layers.map((l) => (
          <div key={l.key} className="surface fp-layer">
            <h4>{l.label} · {l.elements.length}</h4>
            {l.elements.map((e) => (
              <div key={e.node_id} className="fp-el">
                <span className="grow">{e.name}<span className="row-meta"> · {e.node_type.toLowerCase()}</span></span>
                <EvBadge value={e.evidence_class} />
              </div>
            ))}
          </div>
        ))}
      </div>
      {(fp.quality_attributes ?? []).length > 0 && (
        <div className="insp-section">
          <h4>Quality attributes</h4>
          {fp.quality_attributes!.map((q) => (
            <div key={q.attribute} className="finding-card">
              <div className="row-between"><strong>{titleCase(q.attribute)}: {q.target}</strong><EvBadge value={q.evidence_class} /></div>
              <div className="insp-sub">{q.rationale}</div>
            </div>
          ))}
        </div>
      )}
      <button type="button" className="micro-btn" disabled={building} onClick={() => void build()}>{building ? 'Rebuilding…' : 'Rebuild from latest findings'}</button>
      {err && <ErrorNote error={err} />}
    </div>
  );
}

function Alternatives({ w }: { w: WebsiteProfile }) {
  const alts = w.alternatives ?? [];
  if (alts.length === 0) return <Empty>No alternatives proposed yet.{w.model_id ? ' Open the model in X-Ray and use Alternatives to generate a scored set.' : ''}</Empty>;
  return (
    <div className="tab-body alt-grid">
      {alts.map((a) => (
        <div key={a.name} className="surface alt-card">
          <div className="row-between"><strong>{a.name}</strong><EvBadge value="PROPOSED" /></div>
          <p>{a.summary}</p>
          {(a.strengths ?? []).length > 0 && <><div className="section-label">Strengths</div><ul className="insp-list">{a.strengths!.map((x) => <li key={x}>{x}</li>)}</ul></>}
          {(a.risks ?? []).length > 0 && <><div className="section-label">Risks</div><ul className="insp-list">{a.risks!.map((x) => <li key={x}>{x}</li>)}</ul></>}
          {a.best_when && <div className="insp-sub">Best when: {a.best_when}</div>}
        </div>
      ))}
    </div>
  );
}

function KnowledgeUsed({ api, runId }: { api: SysteraApiClient; runId?: string }) {
  const res = useAsync(runId ? () => api.runKnowledge(runId) : null, `wk:${runId}`, 'run knowledge');
  if (!runId) return <Empty>No agent run yet. Start agent analysis from the Lens extension to see which research the agents relied on.</Empty>;
  if (res.error) return <ErrorNote error={res.error} onRetry={res.reload} />;
  if (!res.data) return <Skeleton rows={4} />;
  if (res.data.length === 0) return <Empty>The agents didn’t retrieve any Engineering Pulse knowledge for this run.</Empty>;
  return <div className="tab-body rows">{res.data.map((r) => <RefCard key={r.id} r={r} />)}</div>;
}

function ComparisonView({ c, onClose, onSelect }: { c: WebsiteComparison; onClose: () => void; onSelect: (id: string) => void }) {
  const [onlyDiff, setOnlyDiff] = useState(false);
  const rows = (c.capabilities ?? []).filter((r) => !onlyDiff || r.present_on < c.sites.length);
  return (
    <div className="site-detail">
      <div className="view-head">
        <div>
          <div className="node-type">Comparison · {c.taxonomy_version}</div>
          <h2>{c.sites.map((s) => s.domain).join(' vs ')}</h2>
          <div className="row-meta">{(c.shared_capabilities ?? []).length} shared capabilities</div>
        </div>
        <div className="view-head-actions">
          <label className="check"><input type="checkbox" checked={onlyDiff} onChange={(e) => setOnlyDiff(e.target.checked)} /> Differences only</label>
          <button type="button" className="btn btn-secondary" onClick={onClose}>Close</button>
        </div>
      </div>
      <div className="cf-banner">{c.disclaimer}</div>

      {(c.pairs ?? []).length > 0 && (
        <div className="pair-row">
          {c.pairs!.map((p) => {
            const a = c.sites.find((s) => s.website_id === p.a)?.domain ?? p.a;
            const b = c.sites.find((s) => s.website_id === p.b)?.domain ?? p.b;
            return (
              <div key={`${p.a}${p.b}`} className="surface pair">
                <span className="big-num">{Math.round(p.capability_similarity * 100)}%</span>
                <span className="row-meta">{a} ↔ {b}</span>
                {(p.shared_technology ?? []).length > 0 && <span className="row-meta">tech: {p.shared_technology!.join(', ')}</span>}
              </div>
            );
          })}
        </div>
      )}

      <div className="cf-table-wrap">
        <table className="cf-table matrix">
          <thead>
            <tr><th>Capability</th>{c.sites.map((s) => <th key={s.website_id}><button type="button" className="link-btn" onClick={() => onSelect(s.website_id)}>{s.domain}</button><div className="row-meta">{s.page_count} pages · {s.analysis_mode.replace(/_/g, ' ')}</div></th>)}</tr>
          </thead>
          <tbody>
            {rows.map((r) => (
              <tr key={r.key}>
                <td>{r.name}<div className="row-meta">→ {r.proposed_component}</div></td>
                {c.sites.map((s) => {
                  const cell = r.sites[s.website_id];
                  return <td key={s.website_id} className="center">{cell?.present ? <EvBadge value={cell.evidence_class ?? 'INFERRED'} /> : <span className="muted">—</span>}</td>;
                })}
              </tr>
            ))}
            {(c.technology ?? []).map((t) => (
              <tr key={`t:${t.name}`} className="tech-row">
                <td>{t.name}<div className="row-meta">technology</div></td>
                {c.sites.map((s) => <td key={s.website_id} className="center">{t.sites[s.website_id] ? <span className="tone tone-muted">{t.sites[s.website_id].toLowerCase()}</span> : <span className="muted">—</span>}</td>)}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {(c.notes ?? []).map((n) => <div key={n} className="insp-sub">{n}</div>)}
    </div>
  );
}
