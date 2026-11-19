import { useMemo, useState } from 'react';
import type { SysteraApiClient } from '../lib/api';
import { MATURITIES, SOURCE_TYPES, conflictProblems, defaultCanonicalKey, ingestProblems, titleCase } from '../lib/research';
import type { AccessPolicy, KnowledgeDocument, KnowledgeIngest, KnowledgeResult, SourceType } from '../types/research';
import { ErrorNote, errorText } from './ui';

const BLANK = {
  title: '', canonical_key: '', source_url: '', source_type: 'PRIMARY_DOC' as SourceType, is_primary_source: true, licence: '',
  access_policy: 'TENANT' as AccessPolicy, published_at: '', maturity: 'MAINSTREAM', confidence_band: 'MEDIUM' as const, raw_content: '',
};

/** Add a document to the workspace's knowledge base (POST /v1/knowledge/documents). */
export function AddDocumentForm({ api, onDone, onCancel }: {
  api: SysteraApiClient;
  onDone: (doc: KnowledgeDocument, created: boolean) => void;
  onCancel: () => void;
}) {
  const [f, setF] = useState<Omit<typeof BLANK, 'confidence_band'> & { confidence_band: KnowledgeIngest['confidence_band'] }>(BLANK);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [touched, setTouched] = useState(false);
  const problems = ingestProblems(f);
  const set = <K extends keyof typeof f>(k: K, v: (typeof f)[K]) => setF((p) => ({ ...p, [k]: v }));

  const submit = async () => {
    setTouched(true);
    if (problems.length) return;
    setBusy(true);
    setError(null);
    try {
      const body: KnowledgeIngest = {
        title: f.title.trim(), raw_content: f.raw_content, source_type: f.source_type, is_primary_source: f.is_primary_source,
        access_policy: f.access_policy, maturity: f.maturity, confidence_band: f.confidence_band, collector: 'web-workspace',
        ...(f.canonical_key.trim() ? { canonical_key: f.canonical_key.trim() } : {}),
        ...(f.source_url.trim() ? { source_url: f.source_url.trim() } : {}),
        ...(f.licence.trim() ? { licence: f.licence.trim() } : {}),
        ...(f.published_at ? { published_at: new Date(f.published_at).toISOString() } : {}),
      };
      const r = await api.ingestKnowledge(body);
      setF(BLANK);
      setTouched(false);
      onDone(r.document, r.created);
    } catch (e) {
      setError(errorText(e, 'adding knowledge'));
    } finally {
      setBusy(false);
    }
  };

  return (
    <form className="surface write-form" onSubmit={(e) => { e.preventDefault(); void submit(); }}>
      <div className="row-between"><strong>Add a knowledge document</strong><button type="button" className="micro-btn" onClick={onCancel}>Cancel</button></div>
      <p className="insp-sub">Documents are chunked, embedded and become searchable to agents in this workspace. Adding the same key again creates a new version and supersedes the old one; identical content is deduplicated.</p>
      <div className="form-grid">
        <label className="field span-2"><span>Title *</span><input value={f.title} onChange={(e) => set('title', e.target.value)} placeholder="e.g. Postgres partitioning guide" /></label>
        <label className="field"><span>Key</span><input value={f.canonical_key} onChange={(e) => set('canonical_key', e.target.value)} placeholder={defaultCanonicalKey(f.title) || 'derived from the title'} /></label>
        <label className="field"><span>Source URL</span><input value={f.source_url} onChange={(e) => set('source_url', e.target.value)} placeholder="https://…" /></label>
        <label className="field"><span>Source type</span>
          <select value={f.source_type} onChange={(e) => set('source_type', e.target.value as SourceType)}>{SOURCE_TYPES.map((t) => <option key={t} value={t}>{titleCase(t)}</option>)}</select>
        </label>
        <label className="field"><span>Licence</span><input value={f.licence} onChange={(e) => set('licence', e.target.value)} placeholder="e.g. CC-BY-4.0 (blank = unknown)" /></label>
        <label className="field"><span>Maturity</span>
          <select value={f.maturity} onChange={(e) => set('maturity', e.target.value)}>{MATURITIES.map((m) => <option key={m} value={m}>{titleCase(m)}</option>)}</select>
        </label>
        <label className="field"><span>Confidence</span>
          <select value={f.confidence_band} onChange={(e) => set('confidence_band', e.target.value as KnowledgeIngest['confidence_band'])}>{['HIGH', 'MEDIUM', 'LOW'].map((m) => <option key={m} value={m}>{titleCase(m)}</option>)}</select>
        </label>
        <label className="field"><span>Visibility</span>
          <select value={f.access_policy} onChange={(e) => set('access_policy', e.target.value as AccessPolicy)}>
            <option value="TENANT">Workspace</option><option value="RESTRICTED">Restricted</option><option value="PUBLIC">Public</option>
          </select>
        </label>
        <label className="field"><span>Published</span><input type="date" value={f.published_at} onChange={(e) => set('published_at', e.target.value)} /></label>
        <label className="check span-2"><input type="checkbox" checked={f.is_primary_source} onChange={(e) => set('is_primary_source', e.target.checked)} /> Primary source (official docs, the spec, the vendor’s own release notes)</label>
        <label className="field span-2"><span>Content *</span>
          <textarea rows={8} value={f.raw_content} onChange={(e) => set('raw_content', e.target.value)} placeholder="Paste the text (HTML is stripped). Long documents are split into chunks automatically." />
          <span className="row-meta">{f.raw_content.length.toLocaleString()} characters</span>
        </label>
      </div>
      {touched && problems.length > 0 && <div className="cf-error">{problems.join(' ')}</div>}
      {error && <ErrorNote error={error} />}
      <div className="form-actions"><button type="submit" className="btn btn-primary" disabled={busy}>{busy ? 'Adding…' : 'Add to knowledge'}</button></div>
    </form>
  );
}

/** Mark a document as conflicting with another (POST /v1/knowledge/documents/{id}/conflict). */
export function ConflictForm({ api, doc, onDone }: { api: SysteraApiClient; doc: KnowledgeDocument; onDone: () => void }) {
  const [open, setOpen] = useState(false);
  const [q, setQ] = useState('');
  const [results, setResults] = useState<KnowledgeResult[] | null>(null);
  const [other, setOther] = useState('');
  const [otherTitle, setOtherTitle] = useState('');
  const [note, setNote] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [touched, setTouched] = useState(false);
  const problems = conflictProblems(doc.id, other, note);
  const candidates = useMemo(() => {
    const seen = new Set<string>();
    return (results ?? []).filter((r) => r.document_id !== doc.id && !seen.has(r.document_id) && seen.add(r.document_id));
  }, [doc.id, results]);

  if (!open) {
    return <button type="button" className="btn btn-secondary" onClick={() => { setOpen(true); setQ(doc.title); }}>Mark a conflict…</button>;
  }

  const search = async () => {
    if (!q.trim()) return;
    setError(null);
    try { setResults(await api.searchKnowledge({ query: q.trim(), include_stale: true, limit: 10 })); } catch (e) { setError(errorText(e, 'knowledge search')); }
  };
  const submit = async () => {
    setTouched(true);
    if (problems.length) return;
    setBusy(true);
    setError(null);
    try {
      await api.recordKnowledgeConflict(doc.id, other.trim(), note.trim());
      setOpen(false);
      onDone();
    } catch (e) {
      const msg = errorText(e, 'this document');
      setError(/Nothing found/.test(msg) ? 'Only documents that belong to this workspace can be marked; platform-wide knowledge is read-only.' : msg);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="surface write-form">
      <div className="row-between"><strong>Record a conflict</strong><button type="button" className="micro-btn" onClick={() => setOpen(false)}>Cancel</button></div>
      <p className="insp-sub">Marks this document as conflicted. Search results show the conflict and note, so agents and reviewers weigh both sides.</p>
      <div className="inline-form">
        <input value={q} onChange={(e) => setQ(e.target.value)} onKeyDown={(e) => { if (e.key === 'Enter') { e.preventDefault(); void search(); } }} placeholder="Find the other document…" />
        <button type="button" className="btn btn-secondary" onClick={() => void search()}>Search</button>
      </div>
      {results && candidates.length === 0 && <div className="empty-note">No other documents match.</div>}
      {candidates.length > 0 && (
        <div className="pick-list" role="listbox" aria-label="Conflicting document">
          {candidates.map((r) => (
            <button key={r.document_id} type="button" role="option" aria-selected={other === r.document_id}
              className={`pick ${other === r.document_id ? 'on' : ''}`} onClick={() => { setOther(r.document_id); setOtherTitle(r.title); }}>
              <span className="row-title">{r.title}</span><span className="row-meta">{r.provenance_ref}</span>
            </button>
          ))}
        </div>
      )}
      <label className="field"><span>Conflicting document id</span>
        <input value={other} onChange={(e) => { setOther(e.target.value.trim()); setOtherTitle(''); }} placeholder="Pick above, or paste a UUID" />
        {otherTitle && <span className="row-meta">{otherTitle}</span>}
      </label>
      <label className="field"><span>What do they disagree on?</span>
        <textarea rows={3} value={note} onChange={(e) => setNote(e.target.value)} placeholder="e.g. Recommends a 24 h retry window; the other says 72 h." />
      </label>
      {touched && problems.length > 0 && <div className="cf-error">{problems.join(' ')}</div>}
      {error && <ErrorNote error={error} />}
      <div className="form-actions"><button type="button" className="btn btn-primary" disabled={busy} onClick={() => void submit()}>{busy ? 'Saving…' : 'Mark as conflicting'}</button></div>
    </div>
  );
}
