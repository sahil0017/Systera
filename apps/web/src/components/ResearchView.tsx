import { useMemo, useState } from 'react';
import type { SysteraApiClient } from '../lib/api';
import {
  MATURITIES, OPTION_CRITERIA, RATIONALE_KEYS, ago, blankOption, growthLabel, isStale, optionProblems, scoreSeries, shortId, titleCase,
} from '../lib/research';
import type { Facet, OptionAssessment, OptionComparison, Report, RunReference } from '../types/research';
import { AddDocumentForm, ConflictForm } from './KnowledgeForms';
import { MemoryView } from './MemoryView';
import type { ProjectSummary } from '../types/xray';
import { Empty, ErrorNote, Meter, Skeleton, Spark, Tone, errorText, useAsync } from './ui';

export type ResearchSection = 'trends' | 'reports' | 'knowledge' | 'memory' | 'compare' | 'feed' | 'sources' | 'provenance';

type Detail = { kind: 'topic'; key: string } | { kind: 'report'; id: string } | { kind: 'doc'; id: string } | null;

interface Props {
  api: SysteraApiClient;
  section: ResearchSection;
  onSection: (s: ResearchSection) => void;
  runId: string;
  onRunId: (id: string) => void;
  projects: ProjectSummary[] | null;
  projectId: string;
  onProjectId: (id: string) => void;
}

const SECTIONS: Array<{ id: ResearchSection; label: string; hint: string }> = [
  { id: 'trends', label: 'Trends', hint: 'What engineering is paying attention to' },
  { id: 'reports', label: 'Research reports', hint: 'Validated findings with sources' },
  { id: 'knowledge', label: 'Knowledge search', hint: 'Search what agents can cite' },
  { id: 'memory', label: 'Project memory', hint: 'What agents remember per project' },
  { id: 'compare', label: 'Compare options', hint: 'Choose on engineering criteria' },
  { id: 'feed', label: 'Agent updates', hint: 'What reached which agents' },
  { id: 'sources', label: 'Sources', hint: 'Where research comes from' },
  { id: 'provenance', label: 'Run provenance', hint: 'Knowledge a run used, and why' },
];

export function ResearchView({ api, section, onSection, runId, onRunId, projects, projectId, onProjectId }: Props) {
  const [detail, setDetail] = useState<Detail>(null);
  const open = (d: Detail) => setDetail(d);

  return (
    <main className={`research ${detail ? 'has-detail' : ''}`}>
      <nav className="research-nav" aria-label="Research sections">
        <div className="section-label">Engineering Pulse</div>
        {SECTIONS.slice(0, 2).map((s) => <NavBtn key={s.id} s={s} on={section === s.id} onClick={() => { onSection(s.id); setDetail(null); }} />)}
        <div className="section-label">Knowledge</div>
        {SECTIONS.slice(2, 6).map((s) => <NavBtn key={s.id} s={s} on={section === s.id} onClick={() => { onSection(s.id); setDetail(null); }} />)}
        <div className="section-label">Provenance</div>
        {SECTIONS.slice(6).map((s) => <NavBtn key={s.id} s={s} on={section === s.id} onClick={() => { onSection(s.id); setDetail(null); }} />)}
      </nav>

      <section className="research-main">
        {section === 'trends' && <Trends api={api} onOpen={open} selected={detail} />}
        {section === 'reports' && <Reports api={api} onOpen={open} selected={detail} />}
        {section === 'knowledge' && <Knowledge api={api} onOpen={open} selected={detail} />}
        {section === 'memory' && <MemoryView api={api} projects={projects} projectId={projectId} onProjectId={onProjectId} />}
        {section === 'compare' && <CompareOptions api={api} />}
        {section === 'feed' && <Feed api={api} onOpen={open} />}
        {section === 'sources' && <Sources api={api} />}
        {section === 'provenance' && <Provenance api={api} runId={runId} onRunId={onRunId} onOpen={open} />}
      </section>

      {detail && (
        <aside className="research-detail">
          <button type="button" className="inspector-close" onClick={() => setDetail(null)} aria-label="Close details">×</button>
          {detail.kind === 'topic' && <TopicDetailPanel api={api} topicKey={detail.key} onOpen={open} />}
          {detail.kind === 'report' && <ReportPanel api={api} id={detail.id} onOpen={open} />}
          {detail.kind === 'doc' && <DocumentPanel api={api} id={detail.id} />}
        </aside>
      )}
    </main>
  );
}

function NavBtn({ s, on, onClick }: { s: (typeof SECTIONS)[number]; on: boolean; onClick: () => void }) {
  return (
    <button type="button" className={`nav-item ${on ? 'active' : ''}`} onClick={onClick}>
      <span className="nav-title">{s.label}</span>
      <span className="nav-hint">{s.hint}</span>
    </button>
  );
}

function Head({ title, sub, children }: { title: string; sub?: string; children?: React.ReactNode }) {
  return (
    <div className="view-head">
      <div>
        <h2>{title}</h2>
        {sub && <p className="insp-sub">{sub}</p>}
      </div>
      {children && <div className="view-head-actions">{children}</div>}
    </div>
  );
}

// ---------------------------------------------------------------- Trends

function Trends({ api, onOpen, selected }: { api: SysteraApiClient; onOpen: (d: Detail) => void; selected: Detail }) {
  const [facet, setFacet] = useState<Facet | ''>('');
  const [q, setQ] = useState('');
  const res = useAsync(() => api.listTrends({ facet }), `trends:${facet}`, 'trends');
  const items = (res.data?.items ?? []).filter((t) => !q || `${t.topic.name} ${t.topic.topic_key} ${(t.topic.keywords ?? []).join(' ')}`.toLowerCase().includes(q.toLowerCase()));

  return (
    <>
      <Head title="Trends" sub={res.data?.note ?? 'Attention scores (0–100) per topic. A score measures attention, never quality.'}>
        <div className="seg">
          {(['', 'DOMAIN', 'TECHNOLOGY', 'PATTERN'] as const).map((f) => (
            <button key={f || 'all'} type="button" className={facet === f ? 'on' : ''} onClick={() => setFacet(f)}>{f ? titleCase(f) : 'All'}</button>
          ))}
        </div>
        <input className="mini-search" placeholder="Filter topics…" value={q} onChange={(e) => setQ(e.target.value)} />
      </Head>
      {res.error && <ErrorNote error={res.error} onRetry={res.reload} />}
      {res.loading && !res.data && <Skeleton rows={6} />}
      {res.data && items.length === 0 && <Empty>{q ? 'No topics match.' : 'No trend scores yet. They appear once the research worker has fetched and scored sources.'}</Empty>}
      <div className="rows">
        {items.map((t) => {
          const g = growthLabel(t.score.components.growth);
          const on = selected?.kind === 'topic' && selected.key === t.topic.topic_key;
          return (
            <button key={t.topic.topic_key} type="button" className={`data-row ${on ? 'active' : ''}`} onClick={() => onOpen({ kind: 'topic', key: t.topic.topic_key })}>
              <span className="score-pill" aria-label={`Trend score ${Math.round(t.score.score)}`}>{Math.round(t.score.score)}</span>
              <span className="grow">
                <span className="row-title">{t.topic.name}</span>
                <span className="row-meta">{titleCase(t.topic.facet)} · {t.topic.maturity ? titleCase(t.topic.maturity) : 'maturity unknown'} · scored {ago(t.score.computed_at)}</span>
              </span>
              <span className={`tone tone-${g.tone}`}>{g.text}</span>
              {t.latest_report ? <Tone value={t.latest_report.confidence_band} label={`${t.latest_report.confidence_band.toLowerCase()} confidence`} /> : <span className="tone tone-muted">no report</span>}
              {t.knowledge && <Tone value={isStale(t.knowledge.stale_after) ? 'STALE' : t.knowledge.status} label={isStale(t.knowledge.stale_after) ? 'stale' : 'in knowledge'} />}
            </button>
          );
        })}
      </div>
    </>
  );
}

function TopicDetailPanel({ api, topicKey, onOpen }: { api: SysteraApiClient; topicKey: string; onOpen: (d: Detail) => void }) {
  const res = useAsync(() => api.getTopic(topicKey), `topic:${topicKey}`, 'this topic');
  if (res.error) return <ErrorNote error={res.error} onRetry={res.reload} />;
  if (!res.data) return <Skeleton rows={8} />;
  const d = res.data;
  const series = scoreSeries(d.scores);
  const latest = (d.scores ?? []).slice().sort((a, b) => b.computed_at.localeCompare(a.computed_at))[0];
  return (
    <div className="detail-body">
      <div className="node-type">{titleCase(d.topic.facet)} topic</div>
      <h3>{d.topic.name}</h3>
      {d.topic.description && <p className="insp-sub">{d.topic.description}</p>}
      {(d.topic.keywords?.length ?? 0) > 0 && <div className="chip-wrap">{d.topic.keywords!.map((k) => <span key={k} className="chip static">{k}</span>)}</div>}

      <div className="insp-section">
        <h4>Score history</h4>
        <div className="spark-row"><Spark values={series} w={260} h={44} label={`Score history for ${d.topic.name}`} /><span className="big-num">{latest ? Math.round(latest.score) : '—'}</span></div>
        {latest && (
          <div className="meters">
            <Meter label="Popularity" value={latest.components.popularity} />
            <Meter label="Growth" value={latest.components.growth} />
            <Meter label="Significance" value={latest.components.significance} />
            <Meter label="Reliability" value={latest.components.reliability} />
            <Meter label="Novelty" value={latest.components.novelty} />
          </div>
        )}
        <div className="insp-sub">Score = 0.25 popularity + 0.25 growth + 0.20 significance + 0.20 reliability + 0.10 novelty. Growth 50 means flat.</div>
      </div>

      <div className="insp-section">
        <h4>Research reports ({d.reports?.length ?? 0})</h4>
        {(d.reports ?? []).length === 0 ? <Empty>No report yet for this topic.</Empty> : d.reports!.map((r) => (
          <button key={r.id} type="button" className="row-btn" onClick={() => onOpen({ kind: 'report', id: r.id })}>
            <span className="row-title">{r.title}</span>
            <span className="row-meta"><Tone value={r.status} /> <Tone value={r.confidence_band} /> {ago(r.created_at)}</span>
          </button>
        ))}
      </div>

      <div className="insp-section">
        <h4>Recent items ({d.items?.length ?? 0})</h4>
        {(d.items ?? []).slice(0, 12).map((it) => (
          <a key={it.id} className="row-btn" href={it.url} target="_blank" rel="noreferrer noopener">
            <span className="row-title">{it.title}</span>
            <span className="row-meta">{it.source_name ?? it.source_key ?? 'source'} · {ago(it.published_at ?? it.retrieved_at)}{it.engagement ? ` · ${it.engagement} engagement` : ''}</span>
          </a>
        ))}
        {(d.items ?? []).length === 0 && <Empty>No items collected yet.</Empty>}
      </div>

      {(d.knowledge ?? []).length > 0 && (
        <div className="insp-section">
          <h4>Published knowledge</h4>
          {d.knowledge!.map((k) => (
            <button key={k.id} type="button" className="row-btn" onClick={() => onOpen({ kind: 'doc', id: k.document_id })}>
              <span className="row-title">{k.title ?? k.topic_key}</span>
              <span className="row-meta"><Tone value={isStale(k.stale_after) ? 'STALE' : k.status} /> published {ago(k.published_at)}</span>
            </button>
          ))}
        </div>
      )}
    </div>
  );
}

// ---------------------------------------------------------------- Reports

function Reports({ api, onOpen, selected }: { api: SysteraApiClient; onOpen: (d: Detail) => void; selected: Detail }) {
  const [status, setStatus] = useState('');
  const res = useAsync(() => api.listReports({ status }), `reports:${status}`, 'research reports');
  return (
    <>
      <Head title="Research reports" sub="Each report must pass validation (sources, primary evidence, calibrated confidence) before it is published as knowledge.">
        <div className="seg">
          {['', 'PUBLISHED', 'VALIDATED', 'DRAFT', 'REJECTED', 'STALE'].map((s) => (
            <button key={s || 'all'} type="button" className={status === s ? 'on' : ''} onClick={() => setStatus(s)}>{s ? titleCase(s) : 'All'}</button>
          ))}
        </div>
      </Head>
      {res.error && <ErrorNote error={res.error} onRetry={res.reload} />}
      {res.loading && !res.data && <Skeleton rows={6} />}
      {res.data && res.data.length === 0 && <Empty>No reports{status ? ` with status ${status.toLowerCase()}` : ''} yet.</Empty>}
      <div className="rows">
        {(res.data ?? []).map((r) => <ReportRow key={r.id} r={r} active={selected?.kind === 'report' && selected.id === r.id} onClick={() => onOpen({ kind: 'report', id: r.id })} />)}
      </div>
    </>
  );
}

function ReportRow({ r, active, onClick }: { r: Report; active: boolean; onClick: () => void }) {
  const stale = isStale(r.stale_after);
  return (
    <button type="button" className={`data-row ${active ? 'active' : ''}`} onClick={onClick}>
      <span className="grow">
        <span className="row-title">{r.title}</span>
        <span className="row-meta">{r.topic_key} · {r.evidence?.length ?? 0} source{(r.evidence?.length ?? 0) === 1 ? '' : 's'} · {Math.round(r.confidence * 100)}% confidence · {ago(r.created_at)}</span>
      </span>
      <Tone value={stale ? 'STALE' : r.status} label={stale ? 'stale' : undefined} />
      <Tone value={r.confidence_band} />
    </button>
  );
}

function ReportPanel({ api, id, onOpen }: { api: SysteraApiClient; id: string; onOpen: (d: Detail) => void }) {
  const res = useAsync(() => api.getReport(id), `report:${id}`, 'this report');
  if (res.error) return <ErrorNote error={res.error} onRetry={res.reload} />;
  if (!res.data) return <Skeleton rows={8} />;
  const r = res.data;
  const stale = isStale(r.stale_after);
  const evidence = r.evidence ?? [];
  const primary = evidence.filter((e) => e.is_primary_source).length;
  return (
    <div className="detail-body">
      <div className="node-type">Research report · {r.topic_key}</div>
      <h3>{r.title}</h3>
      <div className="chip-wrap">
        <Tone value={stale ? 'STALE' : r.status} label={stale ? 'stale' : undefined} />
        <Tone value={r.confidence_band} label={`${r.confidence_band.toLowerCase()} · ${Math.round(r.confidence * 100)}%`} />
        {r.maturity && <span className="tone tone-muted">{titleCase(r.maturity)}</span>}
        <span className="tone tone-muted">fresh {ago(r.freshness_at ?? r.created_at)}</span>
      </div>
      <p>{r.summary}</p>
      {stale && <div className="cf-banner">This report is past its stale date ({new Date(r.stale_after).toLocaleDateString()}). Agents only use it if asked to include stale knowledge.</div>}

      <div className="insp-section">
        <h4>Findings</h4>
        {(r.findings ?? []).length === 0 ? <Empty>No findings recorded.</Empty> : r.findings!.map((f, i) => (
          <div key={i} className="finding">
            <span className={`tone ${f.kind === 'ADOPTION' ? 'tone-warn' : 'tone-ok'}`}>{f.kind.toLowerCase()}</span>
            <span>{f.claim}</span>
            {(f.evidence?.length ?? 0) > 0 && <span className="row-meta">cites {f.evidence!.length} source{f.evidence!.length === 1 ? '' : 's'}</span>}
          </div>
        ))}
        <div className="insp-sub">Adoption findings describe popularity; only technical findings support a design choice.</div>
      </div>

      {((r.limitations?.length ?? 0) > 0 || (r.caveats?.length ?? 0) > 0) && (
        <div className="insp-section">
          <h4>Limitations and caveats</h4>
          <ul className="insp-list">
            {(r.limitations ?? []).map((l) => <li key={`l${l}`}>{l}</li>)}
            {(r.caveats ?? []).map((c) => <li key={`c${c}`} className="muted">{c}</li>)}
          </ul>
        </div>
      )}

      <div className="insp-section">
        <h4>Sources · {evidence.length} ({primary} primary)</h4>
        {evidence.map((e) => (
          <a key={e.item_id} className="row-btn" href={e.url} target="_blank" rel="noreferrer noopener">
            <span className="row-title">{e.title}</span>
            <span className="row-meta">
              {e.is_primary_source ? <span className="tone tone-ok">primary</span> : <span className="tone tone-muted">{e.role.toLowerCase()}</span>}
              {' '}{e.source_name} · reliability {Math.round(e.reliability * 100)}% · {e.licence || 'licence unknown'} · {ago(e.published_at ?? e.retrieved_at)}
            </span>
          </a>
        ))}
        {evidence.length === 0 && <Empty>No sources attached.</Empty>}
      </div>

      <div className="insp-section">
        <h4>Validation {r.validation?.passed ? 'passed' : 'not passed'}</h4>
        <div className="checks">
          {(r.validation?.checks ?? []).map((c) => (
            <div key={c.name} className={`check-row ${c.passed ? 'ok' : 'bad'}`} title={c.detail}>
              <span aria-hidden="true">{c.passed ? '✓' : '✕'}</span> {titleCase(c.name)}{c.detail && !c.passed ? <span className="row-meta"> · {c.detail}</span> : null}
            </div>
          ))}
        </div>
      </div>

      {r.knowledge_update && (
        <button type="button" className="btn btn-secondary" onClick={() => onOpen({ kind: 'doc', id: r.knowledge_update!.document_id })}>Open the published knowledge document</button>
      )}
    </div>
  );
}

// ---------------------------------------------------------------- Knowledge

function Knowledge({ api, onOpen, selected }: { api: SysteraApiClient; onOpen: (d: Detail) => void; selected: Detail }) {
  const [q, setQ] = useState('');
  const [submitted, setSubmitted] = useState('');
  const [stale, setStale] = useState(false);
  const [primary, setPrimary] = useState(false);
  const res = useAsync(
    submitted ? () => api.searchKnowledge({ query: submitted, include_stale: stale, primary_only: primary, limit: 25 }) : null,
    `k:${submitted}:${stale}:${primary}`, 'knowledge search');
  const [adding, setAdding] = useState(false);
  const [flash, setFlash] = useState<string | null>(null);

  return (
    <>
      <Head title="Knowledge search" sub="Hybrid keyword and vector search over the knowledge agents may cite. Content is shown as untrusted text.">
        {!adding && <button type="button" className="btn btn-primary" onClick={() => { setAdding(true); setFlash(null); }}>Add document</button>}
      </Head>
      {adding && (
        <AddDocumentForm api={api} onCancel={() => setAdding(false)} onDone={(doc, created) => {
          setAdding(false);
          setFlash(created ? `Added “${doc.title}” (v${doc.version_number}). It is searchable now.` : `“${doc.title}” already exists with identical content, so nothing new was stored.`);
          onOpen({ kind: 'doc', id: doc.id });
          if (submitted) res.reload();
        }} />
      )}
      {flash && <div className="cf-banner">{flash}</div>}
      <form className="search-form" onSubmit={(e) => { e.preventDefault(); setSubmitted(q.trim()); }}>
        <input className="search-input big" autoFocus value={q} onChange={(e) => setQ(e.target.value)} placeholder="e.g. event sourcing, rate limiting, postgres partitioning" />
        <button type="submit" className="btn btn-primary" disabled={!q.trim()}>Search</button>
      </form>
      <div className="filter-row">
        <label className="check"><input type="checkbox" checked={stale} onChange={(e) => setStale(e.target.checked)} /> Include stale</label>
        <label className="check"><input type="checkbox" checked={primary} onChange={(e) => setPrimary(e.target.checked)} /> Primary sources only</label>
      </div>
      {res.error && <ErrorNote error={res.error} onRetry={res.reload} />}
      {res.loading && <Skeleton rows={5} />}
      {!submitted && <Empty>Search for a pattern, technology or problem to see what knowledge exists and how trustworthy it is.</Empty>}
      {submitted && res.data && res.data.length === 0 && <Empty>No knowledge matches “{submitted}”.{!stale ? ' Try including stale results.' : ''}</Empty>}
      <div className="rows">
        {(res.data ?? []).map((k) => (
          <button key={k.chunk_id} type="button" className={`result-card ${selected?.kind === 'doc' && selected.id === k.document_id ? 'active' : ''}`} onClick={() => onOpen({ kind: 'doc', id: k.document_id })}>
            <div className="row-between">
              <span className="row-title">{k.title}</span>
              <span className="score-chip" title={`keyword ${k.keyword_score.toFixed(2)} · vector ${k.vector_score.toFixed(2)}`}>{k.score.toFixed(2)}</span>
            </div>
            <div className="chip-wrap">
              {k.is_primary_source && <span className="tone tone-ok">primary</span>}
              <span className="tone tone-muted">{k.source_type.toLowerCase()}</span>
              <Tone value={k.confidence_band} />
              <Tone value={k.status} />
              {k.maturity && <span className="tone tone-muted">{titleCase(k.maturity)}</span>}
              {(k.conflicts_with?.length ?? 0) > 0 && <span className="tone tone-bad">conflicts with {k.conflicts_with!.length}</span>}
              {k.injection_suspected && <span className="tone tone-bad">possible prompt injection</span>}
            </div>
            <p className="snippet">{k.content.slice(0, 280)}{k.content.length > 280 ? '…' : ''}</p>
            {k.conflict_note && <div className="insp-sub">Conflict: {k.conflict_note}</div>}
            <div className="row-meta">{k.provenance_ref}{k.licence ? ` · ${k.licence}` : ''}</div>
          </button>
        ))}
      </div>
    </>
  );
}

function DocumentPanel({ api, id }: { api: SysteraApiClient; id: string }) {
  const res = useAsync(() => api.getKnowledgeDocument(id), `doc:${id}`, 'this document');
  if (res.error) return <ErrorNote error={res.error} onRetry={res.reload} />;
  if (!res.data) return <Skeleton rows={6} />;
  const d = res.data;
  const rows: Array<[string, string]> = [
    ['Key', d.canonical_key], ['Version', `v${d.version_number}`], ['Source type', d.source_type], ['Primary source', d.is_primary_source ? 'yes' : 'no'],
    ['Licence', d.licence || '—'], ['Access', d.access_policy], ['Maturity', d.maturity ? titleCase(d.maturity) : '—'],
    ['Published', d.published_at ? new Date(d.published_at).toLocaleDateString() : '—'], ['Updated', ago(d.updated_at)], ['Content hash', d.content_hash.slice(0, 16)],
  ];
  return (
    <div className="detail-body">
      <div className="node-type">Knowledge document</div>
      <h3>{d.title}</h3>
      <div className="chip-wrap"><Tone value={d.status} /><Tone value={d.confidence_band} /></div>
      {d.superseded_by && <div className="cf-banner">Superseded by a newer version ({shortId(d.superseded_by)}).</div>}
      <dl className="kv">{rows.map(([k, v]) => <div key={k}><dt>{k}</dt><dd>{v}</dd></div>)}</dl>
      {d.source_url && <a className="btn btn-secondary" href={d.source_url} target="_blank" rel="noreferrer noopener">Open source</a>}
      {d.status === 'CONFLICTED' && <div className="cf-error">Marked as conflicting with other knowledge. Search results show the note.</div>}
      <ConflictForm api={api} doc={d} onDone={res.reload} />
    </div>
  );
}

// ---------------------------------------------------------------- Compare options

function CompareOptions({ api }: { api: SysteraApiClient }) {
  const [options, setOptions] = useState<OptionAssessment[]>([blankOption(), blankOption()]);
  const [result, setResult] = useState<OptionComparison | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const problems = useMemo(() => optionProblems(options), [options]);
  const set = (i: number, patch: Partial<OptionAssessment>) => setOptions((prev) => prev.map((o, j) => (j === i ? { ...o, ...patch } : o)));
  const setWhy = (i: number, k: string, v: string) => setOptions((prev) => prev.map((o, j) => (j === i ? { ...o, rationale: { ...o.rationale, [k]: v } } : o)));

  const run = async () => {
    setBusy(true);
    setError(null);
    try {
      setResult(await api.compareOptions(options.map((o) => ({ ...o, name: o.name.trim() }))));
    } catch (e) {
      setError(errorText(e, 'the comparison'));
    } finally {
      setBusy(false);
    }
  };

  return (
    <>
      <Head title="Compare options" sub="Rank technology options on fit, security, maturity, operational burden and cost. A trend score may only break a tie, never pick the winner." />
      <div className="option-grid">
        {options.map((o, i) => (
          <div key={i} className="surface option-card">
            <div className="row-between">
              <input className="option-name" value={o.name} placeholder={`Option ${i + 1} name`} onChange={(e) => set(i, { name: e.target.value })} aria-label={`Option ${i + 1} name`} />
              {options.length > 2 && <button type="button" className="micro-btn" onClick={() => setOptions((p) => p.filter((_, j) => j !== i))} aria-label={`Remove option ${i + 1}`}>Remove</button>}
            </div>
            <label className="field-inline"><span>Maturity</span>
              <select value={o.maturity} onChange={(e) => set(i, { maturity: e.target.value })}>{MATURITIES.map((m) => <option key={m} value={m}>{titleCase(m)}</option>)}</select>
            </label>
            {OPTION_CRITERIA.map((c) => (
              <label key={c.key} className="field-inline" title={c.hint}><span>{c.label}</span>
                <span className="rating">
                  {[1, 2, 3, 4, 5].map((n) => (
                    <button key={n} type="button" className={o[c.key] === n ? 'on' : ''} onClick={() => set(i, { [c.key]: n } as Partial<OptionAssessment>)} aria-label={`${c.label} ${n}`}>{n}</button>
                  ))}
                </span>
              </label>
            ))}
            <label className="field-inline"><span>Trend score</span>
              <input type="number" min={0} max={100} placeholder="optional" value={o.trend_score ?? ''} onChange={(e) => set(i, { trend_score: e.target.value === '' ? undefined : Number(e.target.value) })} />
            </label>
            <details className="hint">
              <summary>Rationale ({RATIONALE_KEYS.filter((k) => o.rationale[k]?.trim()).length}/{RATIONALE_KEYS.length})</summary>
              {RATIONALE_KEYS.map((k) => (
                <label key={k} className="field"><span>{titleCase(k)}</span>
                  <textarea rows={2} value={o.rationale[k] ?? ''} onChange={(e) => setWhy(i, k, e.target.value)} placeholder={`Why this ${k.replace('_', ' ')} rating?`} />
                </label>
              ))}
            </details>
          </div>
        ))}
        {options.length < 6 && <button type="button" className="surface option-add" onClick={() => setOptions((p) => [...p, blankOption()])}>+ Add option</button>}
      </div>
      <div className="row-between compare-actions">
        <div className="insp-sub">{problems.length ? problems[0] + (problems.length > 1 ? ` (+${problems.length - 1} more)` : '') : 'Ready to compare.'}</div>
        <button type="button" className="btn btn-primary" disabled={busy || problems.length > 0} onClick={() => void run()}>{busy ? 'Comparing…' : 'Compare'}</button>
      </div>
      {error && <ErrorNote error={error} />}
      {result && (
        <div className="surface compare-result">
          <h3>Recommended: {result.recommended || 'none'}</h3>
          <p className="insp-sub">{result.selection_basis}</p>
          <table className="cf-table">
            <thead><tr><th>#</th><th>Option</th><th>Engineering score</th>{Object.keys(result.weights).map((w) => <th key={w}>{titleCase(w)} <span className="row-meta">×{result.weights[w]}</span></th>)}<th>Trend</th></tr></thead>
            <tbody>
              {result.options.map((o) => (
                <tr key={o.name} className={o.name === result.recommended ? 'best-row' : o.excluded ? 'muted' : ''}>
                  <td>{o.excluded ? '—' : o.rank}</td>
                  <td>{o.name}{o.excluded && <div className="row-meta">{o.exclusion_reason}</div>}</td>
                  <td><strong>{o.engineering_score.toFixed(1)}</strong></td>
                  {Object.keys(result.weights).map((w) => <td key={w}>{Math.round(o.criterion_scores[w] ?? 0)}</td>)}
                  <td>{o.trend_score !== undefined ? Math.round(o.trend_score) : '—'}{o.tie_broken_by_trend && <div className="row-meta">broke a tie</div>}</td>
                </tr>
              ))}
            </tbody>
          </table>
          <div className="insp-sub">{result.trend_score_role}</div>
          {(result.warnings ?? []).map((w) => <div key={w} className="cf-banner">{w}</div>)}
        </div>
      )}
    </>
  );
}

// ---------------------------------------------------------------- Feed and sources

function Feed({ api, onOpen }: { api: SysteraApiClient; onOpen: (d: Detail) => void }) {
  const updates = useAsync(() => api.listKnowledgeUpdates(), 'updates', 'knowledge updates');
  const notes = useAsync(() => api.listNotifications(), 'notes', 'notifications');
  const subs = useAsync(() => api.listSubscriptions(), 'subs', 'subscriptions');
  const byAgent = useMemo(() => {
    const m = new Map<string, string[]>();
    for (const s of subs.data ?? []) if (s.active) m.set(s.agent_type, [...(m.get(s.agent_type) ?? []), s.topic_key]);
    return [...m.entries()].sort(([a], [b]) => a.localeCompare(b));
  }, [subs.data]);

  return (
    <>
      <Head title="Agent updates" sub="When research is published as knowledge, agents subscribed to that topic are notified and use it in their next design work." />
      <div className="two-col">
        <div>
          <div className="section-label">Published knowledge</div>
          {updates.error && <ErrorNote error={updates.error} onRetry={updates.reload} />}
          {updates.loading && !updates.data && <Skeleton />}
          {updates.data?.length === 0 && <Empty>Nothing published yet.</Empty>}
          {(updates.data ?? []).map((u) => (
            <button key={u.id} type="button" className="row-btn" onClick={() => onOpen({ kind: 'doc', id: u.document_id })}>
              <span className="row-title">{u.title ?? u.topic_key}</span>
              <span className="row-meta">
                <Tone value={isStale(u.stale_after) ? 'STALE' : u.status} /> {u.confidence_band && <Tone value={u.confidence_band} />} {ago(u.published_at)}
                {(u.notified_agent_types?.length ?? 0) > 0 && ` · sent to ${u.notified_agent_types!.length} agent type${u.notified_agent_types!.length === 1 ? '' : 's'}`}
              </span>
            </button>
          ))}
        </div>
        <div>
          <div className="section-label">Notifications</div>
          {notes.error && <ErrorNote error={notes.error} onRetry={notes.reload} />}
          {notes.data?.length === 0 && <Empty>No agent notifications yet.</Empty>}
          {(notes.data ?? []).slice(0, 20).map((n) => (
            <div key={n.id} className="feed-item">
              <span className="chip static">{titleCase(n.agent_type)}</span>
              <span className="grow">{n.title}</span>
              <span className="row-meta">{ago(n.created_at)}</span>
            </div>
          ))}
          <div className="section-label">Subscriptions</div>
          {subs.error && <ErrorNote error={subs.error} onRetry={subs.reload} />}
          {subs.data?.length === 0 && <Empty>No agent subscriptions configured.</Empty>}
          {byAgent.map(([agent, topics]) => (
            <div key={agent} className="feed-item">
              <span className="chip static">{titleCase(agent)}</span>
              <span className="grow row-meta">{topics.join(', ')}</span>
            </div>
          ))}
        </div>
      </div>
    </>
  );
}

function Sources({ api }: { api: SysteraApiClient }) {
  const res = useAsync(() => api.listSources(), 'sources', 'sources');
  return (
    <>
      <Head title="Sources" sub="Every source is approved with its terms, licence and rate limit before anything is fetched from it." />
      {res.error && <ErrorNote error={res.error} onRetry={res.reload} />}
      {res.loading && !res.data && <Skeleton rows={5} />}
      {res.data?.length === 0 && <Empty>No sources registered.</Empty>}
      {res.data && res.data.length > 0 && (
        <div className="cf-table-wrap">
          <table className="cf-table">
            <thead><tr><th>Source</th><th>Type</th><th>Reliability</th><th>Licence / allowed use</th><th>Status</th><th>Last fetch</th></tr></thead>
            <tbody>
              {res.data.map((s) => (
                <tr key={s.id}>
                  <td>
                    <a href={s.homepage_url} target="_blank" rel="noreferrer noopener">{s.name}</a>
                    {s.is_primary_source && <span className="tone tone-ok">primary</span>}
                    <div className="row-meta">{s.connector} · ≤{s.rate_limit_per_hour}/h</div>
                  </td>
                  <td>{s.source_type.toLowerCase()}</td>
                  <td>{Math.round(s.reliability * 100)}%</td>
                  <td>{s.licence || '—'}<div className="row-meta">{s.allowed_content}{s.attribution_required ? ' · attribution required' : ''}</div>{s.terms_url && <a className="row-meta" href={s.terms_url} target="_blank" rel="noreferrer noopener">terms</a>}</td>
                  <td><Tone value={s.status} />{s.consecutive_failures > 0 && <div className="row-meta" title={s.last_error}>{s.consecutive_failures} failures</div>}</td>
                  <td>{ago(s.last_fetched_at)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </>
  );
}

// ---------------------------------------------------------------- Provenance

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

function Provenance({ api, runId, onRunId, onOpen }: { api: SysteraApiClient; runId: string; onRunId: (id: string) => void; onOpen: (d: Detail) => void }) {
  const [input, setInput] = useState(runId);
  const valid = UUID.test(runId);
  const run = useAsync(valid ? () => api.getRun(runId) : null, `run:${runId}`, 'this run');
  const refs = useAsync(valid ? () => api.runKnowledge(runId) : null, `rk:${runId}`, 'run knowledge');
  return (
    <>
      <Head title="Run provenance" sub="Every piece of knowledge an agent pulled into a design run is recorded with why it matched and what backs it." />
      <form className="search-form" onSubmit={(e) => { e.preventDefault(); onRunId(input.trim()); }}>
        <input className="search-input big" value={input} onChange={(e) => setInput(e.target.value)} placeholder="Design run id (UUID)" />
        <button type="submit" className="btn btn-primary" disabled={!UUID.test(input.trim())}>Show</button>
      </form>
      {!valid && <Empty>Paste a run id, or open one from a website investigation.</Empty>}
      {run.error && <ErrorNote error={run.error} onRetry={run.reload} />}
      {run.data && (
        <div className="surface run-card">
          <div className="row-between"><strong>{run.data.objective}</strong><Tone value={run.data.state} /></div>
          <div className="row-meta">created {ago(run.data.created_at)} · ${run.data.spent_budget_usd.toFixed(2)} of ${run.data.max_budget_usd.toFixed(2)} budget{run.data.failure_reason ? ` · ${run.data.failure_reason}` : ''}</div>
        </div>
      )}
      {refs.error && <ErrorNote error={refs.error} onRetry={refs.reload} />}
      {refs.loading && <Skeleton rows={4} />}
      {valid && refs.data?.length === 0 && <Empty>This run did not retrieve any Pulse knowledge.</Empty>}
      <div className="rows">{(refs.data ?? []).map((r) => <RefCard key={r.id} r={r} onOpen={onOpen} />)}</div>
    </>
  );
}

export function RefCard({ r, onOpen }: { r: RunReference; onOpen?: (d: Detail) => void }) {
  const ex = r.explanation;
  return (
    <div className="result-card static">
      <div className="row-between">
        <span className="row-title">{r.topic_key}</span>
        <span className="score-chip" title="Relevance">{r.relevance_score.toFixed(2)}</span>
      </div>
      <div className="chip-wrap">
        {r.agent_type && <span className="chip static">{titleCase(r.agent_type)}</span>}
        <Tone value={ex.confidence_band} />
        <Tone value={isStale(ex.stale_after) ? 'STALE' : ex.status} />
        {ex.subscribed_domain && <span className="tone tone-muted">subscribed: {ex.subscribed_domain}</span>}
      </div>
      {(ex.why ?? []).length > 0 && <ul className="insp-list">{ex.why!.map((w) => <li key={w}>{w}</li>)}</ul>}
      {(ex.matched_terms ?? []).length > 0 && <div className="row-meta">matched: {ex.matched_terms!.join(', ')}</div>}
      {(ex.supporting_sources ?? []).length > 0 && (
        <div className="src-list">
          {ex.supporting_sources!.map((s) => (
            <a key={s.item_id} href={s.url} target="_blank" rel="noreferrer noopener" className="row-meta">
              {s.is_primary_source ? '● ' : '○ '}{s.title} — {s.source_name}
            </a>
          ))}
        </div>
      )}
      {(ex.limitations ?? []).length > 0 && <div className="insp-sub">Limitations: {ex.limitations!.join('; ')}</div>}
      <div className="insp-sub">{ex.adoption_signal?.note}</div>
      {onOpen && <button type="button" className="link-btn" onClick={() => onOpen({ kind: 'doc', id: r.document_id })}>Open knowledge document</button>}
    </div>
  );
}
