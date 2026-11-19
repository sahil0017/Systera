import { useState } from 'react';
import { ALL_CONSTRAINTS } from '../lib/constraints';
import type { AlternativeDiff, ConstraintId, CounterfactualSet } from '../types/xray';

interface CounterfactualViewProps {
  modelId: string;
  set: CounterfactualSet | null;
  weights: Record<ConstraintId, number>;
  generating: boolean;
  diffResult: AlternativeDiff | null;
  diffLoading: boolean;
  error: string | null;
  onWeightChange: (constraint: ConstraintId, value: number) => void;
  onApplyPreset: (weights: Record<ConstraintId, number>, label: string) => void;
  onGenerate: () => void;
  onDiff: (fromAltId: string, toAltId: string) => void;
}

const PRESETS: Array<{ label: string; weights: Record<ConstraintId, number> }> = [
  {
    label: 'Startup MVP',
    weights: {
      scale: 1, availability: 2, latency: 3, cost: 5,
      team_size: 4, compliance: 1, operational_maturity: 5, migration_complexity: 5,
    },
  },
  {
    label: 'Enterprise Scale',
    weights: {
      scale: 5, availability: 5, latency: 2, cost: 1,
      team_size: 4, compliance: 5, operational_maturity: 1, migration_complexity: 1,
    },
  },
  {
    label: 'Spiky Event-Driven',
    weights: {
      scale: 4.5, availability: 5, latency: 3, cost: 2.5,
      team_size: 3, compliance: 3, operational_maturity: 2.5, migration_complexity: 3.5,
    },
  },
];

export function CounterfactualView({
  modelId,
  set,
  weights,
  generating,
  diffResult,
  diffLoading,
  error,
  onWeightChange,
  onApplyPreset,
  onGenerate,
  onDiff,
}: CounterfactualViewProps) {
  const [fromId, setFromId] = useState('');
  const [toId, setToId] = useState('');

  return (
    <div className="cf-workspace">
      <aside className="sidebar">
        <section className="panel">
          <h2>Quick Presets</h2>
          <div className="pill-group" style={{ marginBottom: 14 }}>
            {PRESETS.map((p) => (
              <button
                key={p.label}
                type="button"
                className="pill"
                onClick={() => onApplyPreset(p.weights, p.label)}
              >
                {p.label}
              </button>
            ))}
          </div>

          <h2>Constraint Weights (8)</h2>
          <p className="insp-sub" style={{ marginBottom: 12 }}>
            Tune weights and materialize viable architectures as separate graph versions + ADR on the Control Plane.
          </p>
          <div className="weight-list">
            {ALL_CONSTRAINTS.map(({ id, label }) => (
              <label className="weight-row" key={id}>
                <div className="weight-label">
                  <span>{label}</span>
                  <span className="layer-count">{(weights[id] ?? 1).toFixed(1)}x</span>
                </div>
                <input
                  type="range"
                  min={0}
                  max={5}
                  step={0.5}
                  value={weights[id] ?? 1}
                  onChange={(e) => onWeightChange(id, Number(e.target.value))}
                />
              </label>
            ))}
          </div>
          <button
            type="button"
            className="btn btn-primary"
            style={{ width: '100%', marginTop: 14 }}
            disabled={!modelId || generating}
            onClick={onGenerate}
          >
            {generating ? 'Generating on API…' : 'Generate Alternatives & ADR'}
          </button>
        </section>
      </aside>

      <section className="cf-main">
        {error && <div className="cf-error">{error}</div>}

        {!modelId ? (
          <div className="empty-state">
            <h3>Connect to an Architecture Model</h3>
            <p>
              Open <strong>Connect API</strong> in the top bar, authenticate with a Bearer token, and load a
              model to compare and materialize counterfactual designs.
            </p>
          </div>
        ) : !set ? (
          <div className="empty-state">
            <h3>No Counterfactual Alternatives Yet</h3>
            <p>
              Pick a preset or tune the 8 constraint weights on the left, then click{' '}
              <strong>Generate Alternatives &amp; ADR</strong> to materialize Modular Monolith, Domain
              Microservices, and Event-Driven Hybrid as distinct graph versions.
            </p>
          </div>
        ) : (
          <>
            <div className="cf-banner">
              <div>
                <strong>Recommended Option:</strong> {set.decision_matrix.recommended_reason}
                {set.decision_id && (
                  <span className="insp-sub" style={{ marginLeft: 10 }}>
                    (Recorded ADR: {set.decision_id})
                  </span>
                )}
              </div>
            </div>

            <div className="cf-cards">
              {set.alternatives.map((alt) => (
                <div key={alt.id} className={`cf-card ${alt.is_recommended ? 'recommended' : ''}`}>
                  <div className="node-card-top">
                    <span className="node-type">Graph v{alt.graph_version}</span>
                    {alt.is_recommended && <span className="badge ev-OBSERVED">RECOMMENDED</span>}
                  </div>
                  <h3>{alt.name}</h3>
                  <div className="cf-score">{alt.weighted_score.toFixed(2)} / 5.00</div>
                  <div className="score-bar-track">
                    <div className="score-bar-fill" style={{ width: `${(alt.weighted_score / 5) * 100}%` }} />
                  </div>
                  <p className="insp-sub">{alt.summary}</p>
                  <div className="cf-when">
                    <strong>When appropriate:</strong> {alt.when_appropriate}
                  </div>
                  <div className="insp-sub" style={{ marginTop: 6 }}>
                    <strong>Pros:</strong> {alt.pros}
                  </div>
                  <div className="insp-sub" style={{ marginTop: 4 }}>
                    <strong>Cons:</strong> {alt.cons}
                  </div>
                </div>
              ))}
            </div>

            <div className="cf-table-wrap">
              <table className="cf-table">
                <thead>
                  <tr>
                    <th>Constraint</th>
                    <th>Weight</th>
                    {set.alternatives.map((a) => (
                      <th key={a.style}>{a.name}</th>
                    ))}
                  </tr>
                </thead>
                <tbody>
                  {set.decision_matrix.rows.map((row) => (
                    <tr key={row.constraint}>
                      <td className="constraint-cell">{row.constraint.replaceAll('_', ' ')}</td>
                      <td>{row.weight.toFixed(1)}x</td>
                      {set.alternatives.map((a) => {
                        const cell = row.scores[a.style];
                        const isBest = row.best_style === a.style;
                        return (
                          <td key={a.style} className={isBest ? 'best-cell' : ''}>
                            <div className="cell-score-head">
                              <strong>{cell.score.toFixed(1)}</strong>
                              <span
                                className={`badge ev-${
                                  cell.rating === 'HIGH'
                                    ? 'OBSERVED'
                                    : cell.rating === 'MEDIUM'
                                      ? 'INFERRED'
                                      : 'PROPOSED'
                                }`}
                              >
                                {cell.rating}
                              </span>
                            </div>
                            <div className="score-bar-track small">
                              <div className="score-bar-fill" style={{ width: `${(cell.score / 5) * 100}%` }} />
                            </div>
                            <div className="insp-sub">{cell.rationale}</div>
                          </td>
                        );
                      })}
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>

            <div className="cf-diff-panel">
              <div className="cf-diff-controls">
                <strong>Compare Alternatives (Canonical Graph Version Diff):</strong>
                <select value={fromId} onChange={(e) => setFromId(e.target.value)}>
                  <option value="">From alternative…</option>
                  {set.alternatives.map((a) => (
                    <option key={a.id} value={a.id}>
                      {a.name} (v{a.graph_version})
                    </option>
                  ))}
                </select>
                <span>→</span>
                <select value={toId} onChange={(e) => setToId(e.target.value)}>
                  <option value="">To alternative…</option>
                  {set.alternatives.map((a) => (
                    <option key={a.id} value={a.id}>
                      {a.name} (v{a.graph_version})
                    </option>
                  ))}
                </select>
                <button
                  type="button"
                  className="btn btn-secondary"
                  disabled={!fromId || !toId || fromId === toId || diffLoading}
                  onClick={() => onDiff(fromId, toId)}
                >
                  {diffLoading ? 'Diffing…' : 'Diff Graph Versions'}
                </button>
              </div>

              {diffResult && (
                <div className="cf-diff-summary">
                  <div>
                    <strong>
                      {diffResult.from_alternative.name} (v{diffResult.graph_diff.from_version}) →{' '}
                      {diffResult.to_alternative.name} (v{diffResult.graph_diff.to_version})
                    </strong>{' '}
                    · Weighted score delta: {diffResult.weighted_delta >= 0 ? '+' : ''}
                    {diffResult.weighted_delta.toFixed(2)}
                  </div>
                  <div className="diff-chips">
                    <span className="badge ev-OBSERVED">+{diffResult.graph_diff.nodes.added.length} nodes</span>
                    <span className="badge sev-HIGH">-{diffResult.graph_diff.nodes.removed.length} nodes</span>
                    <span className="badge ev-INFERRED">~{diffResult.graph_diff.nodes.modified.length} modified</span>
                    <span className="badge ev-OBSERVED">+{diffResult.graph_diff.edges.added.length} edges</span>
                    <span className="badge sev-HIGH">-{diffResult.graph_diff.edges.removed.length} edges</span>
                  </div>
                </div>
              )}
            </div>
          </>
        )}
      </section>
    </div>
  );
}
