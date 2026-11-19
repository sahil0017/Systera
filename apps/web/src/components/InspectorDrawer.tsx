import { LAYER_BY_ID } from '../lib/layers';
import type { ComponentInspection } from '../types/xray';

interface InspectorDrawerProps {
  inspection: ComponentInspection | null;
  loading: boolean;
  error: string | null;
  onSelectNode: (id: string) => void;
  onDrillDown: (id: string) => void;
}

export function InspectorDrawer({
  inspection,
  loading,
  error,
  onSelectNode,
  onDrillDown,
}: InspectorDrawerProps) {
  if (loading) {
    return (
      <aside className="inspector">
        <div className="inspector-empty">Loading component inspection…</div>
      </aside>
    );
  }
  if (error) {
    return (
      <aside className="inspector">
        <div className="inspector-empty">Inspection failed: {error}</div>
      </aside>
    );
  }
  if (!inspection) {
    return (
      <aside className="inspector">
        <div className="inspector-empty">
          <h3>Component Inspector</h3>
          <p>
            Select any component in the 8-layer X-Ray stack to inspect its responsibilities, dependencies,
            interfaces, data ownership, evidence, assumptions, failure modes, and alternatives in one query.
          </p>
        </div>
      </aside>
    );
  }

  const layerMeta = LAYER_BY_ID[inspection.node.layer];
  const dataNodes = [
    ...inspection.data_ownership.owns.map((r) => ({ ...r, access: 'OWNS' })),
    ...inspection.data_ownership.writes.map((r) => ({ ...r, access: 'WRITES' })),
    ...inspection.data_ownership.reads.map((r) => ({ ...r, access: 'READS' })),
  ];

  return (
    <aside className="inspector">
      <div className="inspector-header">
        <div className="node-card-top">
          <span className="node-type" style={{ color: layerMeta.color }}>
            {layerMeta.label} · {inspection.node.type}
          </span>
          <span className={`badge ev-${inspection.node.evidence_state}`}>{inspection.node.evidence_state}</span>
        </div>
        <h3>{inspection.node.name}</h3>
        <div className="node-key">{inspection.node.key}</div>
        <div className="inspector-actions">
          <button
            type="button"
            className="btn btn-primary"
            onClick={() => onDrillDown(inspection.node.id)}
          >
            Drill Down Neighbourhood
          </button>
        </div>
      </div>

      <div className="insp-section">
        <h4>1. Responsibilities</h4>
        <div className="insp-list">
          {inspection.responsibilities.length === 0 ? (
            <div className="empty-note">None recorded.</div>
          ) : (
            inspection.responsibilities.map((r, idx) => (
              <div className="insp-item" key={idx}>
                {r}
              </div>
            ))
          )}
        </div>
      </div>

      <div className="insp-section">
        <h4>
          2. Dependencies ({inspection.dependencies.upstream.length} upstream ·{' '}
          {inspection.dependencies.downstream.length} downstream)
        </h4>
        <div className="insp-list">
          {[...inspection.dependencies.upstream, ...inspection.dependencies.downstream].length === 0 ? (
            <div className="empty-note">No modeled dependencies.</div>
          ) : (
            <>
              {inspection.dependencies.upstream.map((d) => (
                <div
                  key={`up-${d.edge_id}`}
                  className="insp-item clickable"
                  onClick={() => onSelectNode(d.node.id)}
                >
                  <div>
                    <strong>{d.node.name}</strong>{' '}
                    <span className={`badge ev-${d.node.evidence_state}`}>← {d.relation_type}</span>
                  </div>
                  <div className="insp-sub">
                    {d.node.layer}
                    {d.protocol ? ` · ${d.protocol}` : ''}
                    {d.sync_mode ? ` · ${d.sync_mode}` : ''}
                  </div>
                </div>
              ))}
              {inspection.dependencies.downstream.map((d) => (
                <div
                  key={`dn-${d.edge_id}`}
                  className="insp-item clickable"
                  onClick={() => onSelectNode(d.node.id)}
                >
                  <div>
                    <strong>{d.node.name}</strong>{' '}
                    <span className={`badge ev-${d.node.evidence_state}`}>→ {d.relation_type}</span>
                  </div>
                  <div className="insp-sub">
                    {d.node.layer}
                    {d.protocol ? ` · ${d.protocol}` : ''}
                    {d.sync_mode ? ` · ${d.sync_mode}` : ''}
                  </div>
                </div>
              ))}
            </>
          )}
        </div>
      </div>

      <div className="insp-section">
        <h4>3. Interfaces ({inspection.interfaces.length})</h4>
        <div className="insp-list">
          {inspection.interfaces.length === 0 ? (
            <div className="empty-note">No exposed or consumed interfaces.</div>
          ) : (
            inspection.interfaces.map((i, idx) => (
              <div
                key={`${i.target.id}-${idx}`}
                className="insp-item clickable"
                onClick={() => onSelectNode(i.target.id)}
              >
                <div>
                  <strong>{i.role}</strong> · {i.target.name}
                </div>
                <div className="insp-sub">
                  {i.protocol || i.target.type}
                  {i.sync_mode ? ` · ${i.sync_mode}` : ''}
                </div>
              </div>
            ))
          )}
        </div>
      </div>

      <div className="insp-section">
        <h4>4. Data Ownership ({dataNodes.length})</h4>
        <div className="insp-list">
          {dataNodes.length === 0 ? (
            <div className="empty-note">No direct data store ownership or access.</div>
          ) : (
            dataNodes.map((r, idx) => (
              <div
                key={`${r.id}-${idx}`}
                className="insp-item clickable"
                onClick={() => onSelectNode(r.id)}
              >
                <div>
                  <strong>{r.name}</strong> <span className="node-type">{r.access}</span>
                </div>
                <div className="insp-sub">
                  {r.type} · {r.evidence_state}
                </div>
              </div>
            ))
          )}
        </div>
      </div>

      <div className="insp-section">
        <h4>5. Evidence &amp; Claims ({inspection.evidence.length})</h4>
        <div className="insp-list">
          {inspection.evidence.length === 0 ? (
            <div className="empty-note">No linked ledger claims yet.</div>
          ) : (
            inspection.evidence.map((c) => (
              <div className="insp-item" key={c.id}>
                <div>
                  <span className={`badge ev-${c.claim_class}`}>{c.claim_class}</span>{' '}
                  <strong>{c.confidence_band}</strong>
                </div>
                <div style={{ marginTop: 4 }}>{c.statement}</div>
                {c.evidence.map((ev) => (
                  <div className="insp-sub" key={ev.id}>
                    Source: {ev.source_ref} ({ev.trust_level})
                  </div>
                ))}
              </div>
            ))
          )}
        </div>
      </div>

      <div className="insp-section">
        <h4>6. Assumptions ({inspection.assumptions.length})</h4>
        <div className="insp-list">
          {inspection.assumptions.length === 0 ? (
            <div className="empty-note">No explicit assumptions linked.</div>
          ) : (
            inspection.assumptions.map((a) => (
              <div className="insp-item" key={a.id}>
                <div>
                  <span className={`badge sev-${a.risk_if_wrong}`}>RISK: {a.risk_if_wrong}</span> {a.statement}
                </div>
                <div className="insp-sub">Validation: {a.validation_method || a.rationale}</div>
              </div>
            ))
          )}
        </div>
      </div>

      <div className="insp-section">
        <h4>7. Failure Modes ({inspection.failure_modes.length})</h4>
        <div className="insp-list">
          {inspection.failure_modes.length === 0 ? (
            <div className="empty-note">No failure modes identified.</div>
          ) : (
            inspection.failure_modes.map((f, idx) => (
              <div className="insp-item" key={idx}>
                <div>
                  <span className={`badge sev-${f.severity}`}>{f.severity}</span> <strong>{f.title}</strong>
                </div>
                <div className="insp-sub">
                  {f.description}
                  {f.mitigation ? ` Mitigation: ${f.mitigation}` : ''}
                </div>
              </div>
            ))
          )}
        </div>
      </div>

      <div className="insp-section">
        <h4>8. Alternatives ({inspection.alternatives.length})</h4>
        <div className="insp-list">
          {inspection.alternatives.length === 0 ? (
            <div className="empty-note">No ADR alternatives recorded for this component.</div>
          ) : (
            inspection.alternatives.map((a, idx) => (
              <div className="insp-item" key={idx}>
                <div>
                  <strong>
                    {a.adr_number ? `ADR-${a.adr_number}: ` : ''}
                    {a.title}
                  </strong>
                </div>
                {a.chosen_option && <div className="insp-sub">Chosen: {a.chosen_option}</div>}
                <div className="insp-sub">
                  Rejected Option: {a.option}
                  {a.rejection_reason ? ` — ${a.rejection_reason}` : ''}
                </div>
              </div>
            ))
          )}
        </div>
      </div>
    </aside>
  );
}
