import { useMemo, useState } from 'react';
import type { SysteraApiClient } from '../lib/api';
import { LAYER_BY_ID } from '../lib/layers';
import { titleCase } from '../lib/research';
import type { XRayLayerId } from '../types/xray';
import { Empty, ErrorNote, Skeleton, useAsync } from './ui';

interface Props {
  api: SysteraApiClient;
  modelId: string;
  version: string;
  onLearnComponent: (nodeId: string) => void;
}

const LEVELS = ['FOUNDATIONAL', 'INTERMEDIATE', 'ADVANCED'];

/** Whole-model learning: a component tour, interview questions and hands-on exercises. */
export function LearnOverview({ api, modelId, version, onLearnComponent }: Props) {
  const res = useAsync(() => api.getLearningOverview(modelId, { version }), `lo:${modelId}:${version}`, 'learning mode');
  const [level, setLevel] = useState('');
  const [revealed, setRevealed] = useState<Set<number>>(new Set());
  const [q, setQ] = useState('');

  const byLayer = useMemo(() => {
    const m = new Map<string, NonNullable<NonNullable<typeof res.data>['components']>>();
    for (const c of res.data?.components ?? []) {
      if (q && !`${c.name} ${c.type} ${c.one_liner}`.toLowerCase().includes(q.toLowerCase())) continue;
      m.set(c.layer, [...(m.get(c.layer) ?? []), c]);
    }
    return [...m.entries()];
  }, [q, res.data]);

  if (res.error) return <main className="learn-view"><ErrorNote error={res.error} onRetry={res.reload} /></main>;
  if (!res.data) return <main className="learn-view"><Skeleton rows={8} /></main>;
  const d = res.data;
  const questions = (d.interview_questions ?? []).filter((x) => !level || x.level === level);

  return (
    <main className="learn-view">
      <div className="view-head">
        <div>
          <h2>Learn this architecture</h2>
          <p className="insp-sub">{d.disclaimer} · graph v{d.graph_version}</p>
        </div>
      </div>

      <div className="learn-grid">
        <section>
          <div className="row-between">
            <div className="section-label">Component tour · {d.components?.length ?? 0}</div>
            <input className="mini-search" placeholder="Filter…" value={q} onChange={(e) => setQ(e.target.value)} />
          </div>
          {byLayer.length === 0 && <Empty>{q ? 'No components match.' : 'This model has no components yet.'}</Empty>}
          {byLayer.map(([layer, comps]) => (
            <div key={layer} className="tour-layer">
              <div className="tour-layer-head">
                <span className="layer-swatch" style={{ background: LAYER_BY_ID[layer as XRayLayerId]?.color }} />
                {LAYER_BY_ID[layer as XRayLayerId]?.label ?? titleCase(layer)}
              </div>
              {comps.map((c) => (
                <button key={c.node_id} type="button" className="row-btn" onClick={() => onLearnComponent(c.node_id)} title="Open the full lesson">
                  <span className="row-title">{c.name} <span className="row-meta">· {c.type.toLowerCase()}</span></span>
                  <span className="row-meta">{c.one_liner}</span>
                </button>
              ))}
            </div>
          ))}
        </section>

        <section>
          <div className="row-between">
            <div className="section-label">Interview questions · {d.interview_questions?.length ?? 0}</div>
            <div className="seg">
              <button type="button" className={!level ? 'on' : ''} onClick={() => setLevel('')}>All</button>
              {LEVELS.map((l) => <button key={l} type="button" className={level === l ? 'on' : ''} onClick={() => setLevel(l)}>{titleCase(l)}</button>)}
            </div>
          </div>
          {questions.length === 0 && <Empty>No questions at this level.</Empty>}
          {questions.map((x) => {
            const i = (d.interview_questions ?? []).indexOf(x);
            const open = revealed.has(i);
            return (
              <div key={i} className="surface q-card">
                <span className={`tone tone-${x.level === 'ADVANCED' ? 'bad' : x.level === 'INTERMEDIATE' ? 'warn' : 'ok'}`}>{x.level.toLowerCase()}</span>
                <p>{x.question}</p>
                {open ? (
                  <ul className="insp-list">{(x.good_answers_cover ?? []).map((g) => <li key={g}>{g}</li>)}</ul>
                ) : (
                  <button type="button" className="link-btn" onClick={() => setRevealed((s) => new Set(s).add(i))}>Show what a good answer covers</button>
                )}
              </div>
            );
          })}

          <div className="section-label">Exercises · {d.exercises?.length ?? 0}</div>
          {(d.exercises ?? []).map((e) => (
            <details key={e.title} className="surface q-card">
              <summary><strong>{e.title}</strong> <span className="tone tone-muted">{e.difficulty.toLowerCase()}</span></summary>
              <p>{e.goal}</p>
              {(e.steps ?? []).length > 0 && <ol className="insp-list">{e.steps!.map((s) => <li key={s}>{s}</li>)}</ol>}
              {(e.acceptance_criteria ?? []).length > 0 && (
                <>
                  <div className="section-label">Done when</div>
                  <ul className="insp-list">{e.acceptance_criteria!.map((s) => <li key={s}>{s}</li>)}</ul>
                </>
              )}
            </details>
          ))}
        </section>
      </div>
    </main>
  );
}
