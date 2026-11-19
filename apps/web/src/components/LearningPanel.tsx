import { useEffect, useState } from 'react';
import type { SysteraApiClient } from '../lib/api';
import type { LearningChallenge, Lesson } from '../types/xray';

// Engineering Learning Mode (spec §2.7, §8.5): why the component exists, alternatives and
// trade-offs, failure walkthroughs, interview questions, exercises, linked Engineering Pulse
// research, and challenge -> revised explanation.

interface LearningPanelProps {
  api: SysteraApiClient;
  modelId: string;
  nodeId: string;
  version: string;
}

const SUGGESTED = ['Why not a simpler design?', 'What happens when this fails?', 'Will this scale?', 'Is this worth the cost?'];

export function LearningPanel({ api, modelId, nodeId, version }: LearningPanelProps) {
  const [lesson, setLesson] = useState<Lesson | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [text, setText] = useState('');
  const [sending, setSending] = useState(false);
  const [latest, setLatest] = useState<LearningChallenge | null>(null);

  useEffect(() => {
    let live = true;
    setLesson(null);
    setError(null);
    setLatest(null);
    api
      .getLesson(modelId, nodeId, { version })
      .then((l) => live && setLesson(l))
      .catch((e: Error) => live && setError(e.message));
    return () => {
      live = false;
    };
  }, [api, modelId, nodeId, version]);

  const challenge = async (value: string) => {
    if (!value.trim()) return;
    setSending(true);
    setError(null);
    try {
      const c = await api.challengeLesson(modelId, nodeId, value, { version });
      setLatest(c);
      setText('');
      setLesson((l) => (l ? { ...l, challenges: [c, ...(l.challenges ?? [])] } : l));
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setSending(false);
    }
  };

  if (error && !lesson) return <div className="cf-error">Learning Mode failed: {error}</div>;
  if (!lesson) return <div className="empty-note">Preparing the lesson…</div>;

  return (
    <div className="learning">
      <div className="empty-note">{lesson.disclaimer}</div>

      <div className="insp-section">
        <h4>Why it exists</h4>
        <p className="learn-lead">{lesson.why_it_exists.summary}</p>
        <ul className="learn-list">{(lesson.why_it_exists.details ?? []).map((d, i) => <li key={i}>{d}</li>)}</ul>
      </div>

      <div className="insp-section">
        <h4>Problem it solves</h4>
        <p className="learn-lead">{lesson.problem_solved.summary}</p>
        <ul className="learn-list">{(lesson.problem_solved.details ?? []).map((d, i) => <li key={i}>{d}</li>)}</ul>
      </div>

      <div className="insp-section">
        <h4>Alternatives &amp; trade-offs ({lesson.alternatives.length})</h4>
        {lesson.alternatives.map((a, i) => (
          <div className="insp-item" key={i}>
            <div>
              <strong>{a.option}</strong> <span className="learn-src">{a.source}</span>
            </div>
            <div className="insp-sub">When to choose: {a.when_to_choose}</div>
            {a.pros && <div className="insp-sub">+ {a.pros}</div>}
            {a.cons && <div className="insp-sub">− {a.cons}</div>}
          </div>
        ))}
      </div>

      <div className="insp-section">
        <h4>Failure walkthroughs ({lesson.failure_scenarios.length})</h4>
        {lesson.failure_scenarios.map((f, i) => (
          <details className="insp-item" key={i}>
            <summary>
              <span className={`sev sev-${f.severity}`}>{f.severity}</span> {f.scenario}
            </summary>
            <div className="insp-sub"><strong>Detect:</strong> {f.detection}</div>
            <div className="insp-sub"><strong>Impact:</strong> {f.impact}</div>
            <div className="insp-sub"><strong>Recover:</strong> {f.recovery}</div>
          </details>
        ))}
      </div>

      <div className="insp-section">
        <h4>Interview questions ({lesson.interview_questions.length})</h4>
        {lesson.interview_questions.map((q, i) => (
          <details className="insp-item" key={i}>
            <summary>
              <span className="learn-src">{q.level}</span> {q.question}
            </summary>
            <div className="insp-sub">A strong answer covers: {q.good_answers_cover.join(' · ')}</div>
          </details>
        ))}
      </div>

      <div className="insp-section">
        <h4>Hands-on exercises ({lesson.exercises.length})</h4>
        {lesson.exercises.map((x, i) => (
          <details className="insp-item" key={i}>
            <summary>
              <span className="learn-src">{x.difficulty}</span> {x.title}
            </summary>
            <div className="insp-sub">{x.goal}</div>
            <ol className="learn-list">{x.steps.map((s, j) => <li key={j}>{s}</li>)}</ol>
            <div className="insp-sub"><strong>Done when:</strong> {x.acceptance_criteria.join(' ')}</div>
          </details>
        ))}
      </div>

      <div className="insp-section">
        <h4>Engineering Pulse research</h4>
        {lesson.research.length === 0 ? (
          <div className="empty-note">
            {lesson.research_status === 'UNAVAILABLE'
              ? 'Engineering Pulse is not available right now.'
              : 'No validated Engineering Pulse research matches this component yet.'}
          </div>
        ) : (
          lesson.research.map((r) => (
            <div className="insp-item" key={r.provenance_ref}>
              <div>
                <strong>{r.title}</strong> <span className="learn-src">{r.confidence_band} · {r.maturity}</span>
              </div>
              {(r.why ?? []).map((w, i) => <div className="insp-sub" key={i}>{w}</div>)}
              {r.sources.map((s) => (
                <div className="insp-sub" key={s.url}>
                  <a href={s.url} target="_blank" rel="noreferrer">{s.title}</a> ({s.source}{s.is_primary ? ', primary' : ''})
                </div>
              ))}
              <div className="insp-sub">{r.adoption_note}</div>
            </div>
          ))
        )}
      </div>

      <div className="insp-section">
        <h4>Challenge this design</h4>
        <div className="learn-suggest">
          {SUGGESTED.map((s) => (
            <button key={s} type="button" className="micro-btn" disabled={sending} onClick={() => void challenge(s)}>{s}</button>
          ))}
        </div>
        <textarea
          className="learn-input"
          value={text}
          maxLength={2000}
          placeholder={`e.g. "Why not keep ${lesson.node_name} inside a modular monolith?"`}
          onChange={(e) => setText(e.target.value)}
        />
        <button type="button" className="btn btn-primary" disabled={sending || !text.trim()} onClick={() => void challenge(text)}>
          {sending ? 'Revising…' : 'Ask for a revised explanation'}
        </button>
        {error && <div className="cf-error">{error}</div>}
        {(latest ? [latest, ...lesson.challenges.filter((c) => c.id !== latest.id)] : lesson.challenges).map((c) => (
          <div className="insp-item learn-revision" key={c.id}>
            <div className="insp-sub">Revision {c.revision} · “{c.text}” · <span className="learn-src">{c.revised.concern}</span></div>
            <div><strong>{c.revised.summary}</strong></div>
            <ul className="learn-list">{(c.revised.response ?? []).map((r, i) => <li key={i}>{r}</li>)}</ul>
            {c.revised.relevant_failures.length > 0 && (
              <ul className="learn-list">{c.revised.relevant_failures.map((r, i) => <li key={i}>{r}</li>)}</ul>
            )}
            <div className="insp-sub"><strong>When you are right:</strong> {c.revised.when_you_are_right}</div>
            {c.revised.consider_instead.length > 0 && (
              <div className="insp-sub">Consider: {c.revised.consider_instead.map((a) => a.option).join(', ')}</div>
            )}
            {(c.revised.evidence_to_decide ?? []).length > 0 && (
              <div className="insp-sub">Evidence that would settle it: {(c.revised.evidence_to_decide ?? []).join(' · ')}</div>
            )}
            <div className="insp-sub">{c.revised.suggested_next_step}</div>
          </div>
        ))}
      </div>
    </div>
  );
}
