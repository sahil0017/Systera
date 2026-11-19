import { useEffect, useState } from 'react';
import type { SysteraApiClient } from '../lib/api';
import { MAX_MEMORY_SUMMARY_BYTES, MEMORY_TYPES, ago, memoryProblems, splitRefs, titleCase, utf8Bytes } from '../lib/research';
import type { MemoryRecord, MemoryType } from '../types/research';
import type { ProjectSummary } from '../types/xray';
import { Empty, ErrorNote, Skeleton, Tone, errorText, useAsync } from './ui';

interface Props {
  api: SysteraApiClient;
  projects: ProjectSummary[] | null;
  projectId: string;
  onProjectId: (id: string) => void;
}

/** Distilled project memory agents recall in later runs (GET/POST /v1/projects/{id}/memory). */
export function MemoryView({ api, projects, projectId, onProjectId }: Props) {
  const [q, setQ] = useState('');
  const [submitted, setSubmitted] = useState('');
  const [typeFilter, setTypeFilter] = useState<'' | MemoryType>('');
  const [adding, setAdding] = useState(false);
  const [flash, setFlash] = useState<string | null>(null);
  const list = useAsync(projectId ? () => api.projectMemory(projectId, submitted) : null, `mem:${projectId}:${submitted}`, 'project memory');

  useEffect(() => {
    if (!projectId && projects && projects.length > 0) onProjectId(projects[0].id);
  }, [onProjectId, projectId, projects]);

  const items = (list.data ?? []).filter((m) => !typeFilter || m.memory_type === typeFilter);
  const project = projects?.find((p) => p.id === projectId);

  return (
    <>
      <div className="view-head">
        <div>
          <h2>Project memory</h2>
          <p className="insp-sub">Short, distilled notes that agents recall in later runs: decisions, constraints and lessons. Never full transcripts. Workspace-wide notes appear in every project.</p>
        </div>
        <div className="view-head-actions">
          <select className="version-select" value={projectId} onChange={(e) => { onProjectId(e.target.value); setSubmitted(''); setQ(''); }} aria-label="Project">
            {!projects && <option value="">Loading projects…</option>}
            {projects?.length === 0 && <option value="">No projects</option>}
            {(projects ?? []).map((p) => <option key={p.id} value={p.id}>{p.name}</option>)}
          </select>
          {projectId && !adding && <button type="button" className="btn btn-primary" onClick={() => { setAdding(true); setFlash(null); }}>Add memory</button>}
        </div>
      </div>

      {adding && projectId && (
        <MemoryForm api={api} projectId={projectId} projectName={project?.name ?? 'this project'} onCancel={() => setAdding(false)}
          onSaved={(rec) => { setAdding(false); setFlash(`Saved ${rec.memory_type.toLowerCase()} memory.`); list.reload(); }} />
      )}
      {flash && <div className="cf-banner">{flash}</div>}

      <form className="search-form" onSubmit={(e) => { e.preventDefault(); setSubmitted(q.trim()); }}>
        <input className="search-input big" value={q} onChange={(e) => setQ(e.target.value)} placeholder="Recall by meaning, e.g. “why did we pick Postgres”" disabled={!projectId} />
        <button type="submit" className="btn btn-secondary" disabled={!projectId}>Recall</button>
        {submitted && <button type="button" className="btn btn-ghost" onClick={() => { setQ(''); setSubmitted(''); }}>Show latest</button>}
      </form>
      <div className="seg type-seg">
        <button type="button" className={!typeFilter ? 'on' : ''} onClick={() => setTypeFilter('')}>All</button>
        {MEMORY_TYPES.map((t) => <button key={t.id} type="button" className={typeFilter === t.id ? 'on' : ''} onClick={() => setTypeFilter(t.id)} title={t.hint}>{t.label}</button>)}
      </div>

      {!projectId && projects?.length === 0 && <Empty>Create a project first; memory always belongs to one.</Empty>}
      {list.error && <ErrorNote error={list.error} onRetry={list.reload} />}
      {list.loading && !list.data && <Skeleton rows={4} />}
      {list.data && items.length === 0 && <Empty>{submitted ? `Nothing recalled for “${submitted}”.` : 'No memory yet for this project.'}</Empty>}
      <div className="rows">{items.map((m) => <MemoryCard key={m.id} m={m} projectId={projectId} />)}</div>
      {list.data && list.data.length >= 10 && <div className="insp-sub">Showing the 10 most relevant. Recall with a query to find older notes.</div>}
    </>
  );
}

function MemoryCard({ m, projectId }: { m: MemoryRecord; projectId: string }) {
  const shared = !m.project_id || m.project_id !== projectId;
  return (
    <div className="result-card static">
      <div className="chip-wrap">
        <span className="chip static">{titleCase(m.memory_type)}</span>
        {shared ? <span className="tone tone-warn">workspace-wide</span> : <span className="tone tone-muted">this project</span>}
        <span className="tone tone-muted">relevance {m.relevance_score.toFixed(2)}</span>
        {m.expires_at && <Tone value={Date.parse(m.expires_at) < Date.now() ? 'STALE' : 'PENDING'} label={`expires ${new Date(m.expires_at).toLocaleDateString()}`} />}
        <span className="row-meta">{ago(m.created_at)}</span>
      </div>
      <p className="memory-text">{m.summary}</p>
      {(m.source_refs?.length ?? 0) > 0 && <div className="row-meta">sources: {m.source_refs!.join(', ')}</div>}
      {m.run_id && <div className="row-meta mono">run {m.run_id}</div>}
    </div>
  );
}

function MemoryForm({ api, projectId, projectName, onSaved, onCancel }: {
  api: SysteraApiClient; projectId: string; projectName: string; onSaved: (r: MemoryRecord) => void; onCancel: () => void;
}) {
  const [type, setType] = useState<MemoryType>('PROJECT');
  const [summary, setSummary] = useState('');
  const [refs, setRefs] = useState('');
  const [runId, setRunId] = useState('');
  const [relevance, setRelevance] = useState(0.5);
  const [shared, setShared] = useState(false);
  const [expires, setExpires] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [touched, setTouched] = useState(false);
  const problems = memoryProblems({ summary, run_id: runId, relevance, memory_type: type, expires_at: expires || undefined });
  const bytes = utf8Bytes(summary.trim());

  const submit = async () => {
    setTouched(true);
    if (problems.length) return;
    setBusy(true);
    setError(null);
    try {
      const rec = await api.recordMemory(projectId, {
        memory_type: type, summary: summary.trim(), relevance_score: relevance, access_policy: shared ? 'TENANT' : 'PROJECT',
        ...(splitRefs(refs).length ? { source_refs: splitRefs(refs) } : {}),
        ...(runId.trim() ? { run_id: runId.trim() } : {}),
        ...(expires ? { expires_at: new Date(expires).toISOString() } : {}),
      });
      onSaved(rec);
    } catch (e) {
      const msg = errorText(e, 'this project or run');
      setError(/Nothing found/.test(msg) ? 'That run does not belong to this project.' : msg);
    } finally {
      setBusy(false);
    }
  };

  return (
    <form className="surface write-form" onSubmit={(e) => { e.preventDefault(); void submit(); }}>
      <div className="row-between"><strong>Add memory to {projectName}</strong><button type="button" className="micro-btn" onClick={onCancel}>Cancel</button></div>
      <div className="type-picker" role="radiogroup" aria-label="Memory type">
        {MEMORY_TYPES.map((t) => (
          <button key={t.id} type="button" role="radio" aria-checked={type === t.id} className={`type-opt ${type === t.id ? 'on' : ''}`} onClick={() => setType(t.id)}>
            <strong>{t.label}</strong><span>{t.hint}</span>
          </button>
        ))}
      </div>
      <label className="field"><span>Summary *</span>
        <textarea rows={4} value={summary} onChange={(e) => setSummary(e.target.value)} placeholder="e.g. Chose Postgres over DynamoDB because reporting needs ad-hoc joins; revisit if write volume passes 5k/s." />
        <span className={`row-meta ${bytes > MAX_MEMORY_SUMMARY_BYTES ? 'over' : ''}`}>{bytes.toLocaleString()} / {MAX_MEMORY_SUMMARY_BYTES.toLocaleString()} bytes</span>
      </label>
      <div className="form-grid">
        <label className="field"><span>Sources</span><input value={refs} onChange={(e) => setRefs(e.target.value)} placeholder="ADR-12, https://…, comma separated" /></label>
        <label className="field"><span>Run id</span><input value={runId} onChange={(e) => setRunId(e.target.value)} placeholder="optional, a run in this project" /></label>
        <label className="field"><span>Relevance · {relevance.toFixed(2)}</span><input type="range" min={0.05} max={1} step={0.05} value={relevance} onChange={(e) => setRelevance(Number(e.target.value))} /></label>
        <label className="field"><span>Expires{type === 'WORKING' ? ' *' : ''}</span><input type="date" value={expires} onChange={(e) => setExpires(e.target.value)} /></label>
        <label className="check span-2"><input type="checkbox" checked={shared} onChange={(e) => setShared(e.target.checked)} /> Share with every project in this workspace</label>
      </div>
      {touched && problems.length > 0 && <div className="cf-error">{problems.join(' ')}</div>}
      {error && <ErrorNote error={error} />}
      <div className="form-actions"><button type="submit" className="btn btn-primary" disabled={busy}>{busy ? 'Saving…' : 'Save memory'}</button></div>
    </form>
  );
}
