import { useEffect, useState } from 'react';
import type { SysteraApiClient } from '../lib/api';
import { LAYER_BY_ID } from '../lib/layers';
import { LearningPanel } from './LearningPanel';
import type { ComponentInspection } from '../types/xray';

export type InspectorTab = 'all' | 'overview' | 'connections' | 'evidence' | 'alternatives' | 'learn';

interface InspectorDrawerProps {
  inspection: ComponentInspection | null;
  loading: boolean;
  error: string | null;
  onSelectNode: (id: string) => void;
  onDrillDown: (id: string) => void;
  onCopy: (text: string, label: string) => void;
  /** Enables the Learning Mode tab (spec §2.7). */
  learning?: { api: SysteraApiClient; modelId: string; version: string };
  /** Tab to switch to; re-applied whenever `tabRequest.seq` changes. */
  tabRequest?: { tab: InspectorTab; seq: number };
}

export function InspectorDrawer({
  inspection,
  loading,
  error,
  onSelectNode,
  onDrillDown,
  onCopy,
  learning,
  tabRequest,
}: InspectorDrawerProps) {
  const [tab, setTab] = useState<InspectorTab>(tabRequest?.tab ?? 'all');
  useEffect(() => {
    if (tabRequest) setTab(tabRequest.tab);
  }, [tabRequest]);

  if (loading) {
    return (
      <aside className="inspector">
        <div className="skeleton-wrap">
          <div className="skeleton-line wide" />
          <div className="skeleton-line short" />
          <div className="skeleton-box" />
          <div className="skeleton-box" />
          <div className="skeleton-box" />
        </div>
      </aside>
    );
  }
  if (error) {
    return (
      <aside className="inspector">
        <div className="cf-error" style={{ marginTop: 12 }}>
          Inspection failed: {error}
        </div>
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
          <div className="kbd-hints">
            <div><kbd>/</kbd> Focus search</div>
            <div><kbd>Esc</kbd> Exit drill-down / clear selection</div>
          </div>
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
  const depCount = inspection.dependencies.upstream.length + inspection.dependencies.downstream.length;
  const riskCount =
    inspection.evidence.length + inspection.assumptions.length + inspection.failure_modes.length;

  // Learning Mode is its own view (it loads a lesson), so "All" does not include it.
  const show = (section: Exclude<InspectorTab, 'all' | 'learn'>) => tab === 'all' || tab === section;

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
        <div className="key-row">
          <span className="node-key">{inspection.node.key}</span>
          <button
            type="button"
            className="micro-btn"
            onClick={() => onCopy(inspection.node.id, 'Node ID')}
            title="Copy Node UUID"
          >
            Copy ID
          </button>
        </div>
        <div className="inspector-actions">
          <button
            type="button"
            className="btn btn-primary"
            onClick={() => onDrillDown(inspection.node.id)}
          >
            Drill Down Neighbourhood
          </button>
        </div>

        <div className="insp-tabs">
          <button type="button" className={`insp-tab ${tab === 'all' ? 'active' : ''}`} onClick={() => setTab('all')}>
            All
          </button>
          <button type="button" className={`insp-tab ${tab === 'overview' ? 'active' : ''}`} onClick={() => setTab('overview')}>
            Overview
          </button>
          <button type="button" className={`insp-tab ${tab === 'connections' ? 'active' : ''}`} onClick={() => setTab('connections')}>
            Links ({depCount + inspection.interfaces.length})
          </button>
          <button type="button" className={`insp-tab ${tab === 'evidence' ? 'active' : ''}`} onClick={() => setTab('evidence')}>
            Risk ({riskCount})
          </button>
          <button type="button" className={`insp-tab ${tab === 'alternatives' ? 'active' : ''}`} onClick={() => setTab('alternatives')}>
            ADRs ({inspection.alternatives.length})
          </button>
          {learning && (
            <button type="button" className={`insp-tab ${tab === 'learn' ? 'active' : ''}`} onClick={() => setTab('learn')}>
              Learn
            </button>
          )}
        </div>
      </div>

      {show('overview') && (
        <>
          <div className="insp-section">
            <h4>Responsibilities</h4>
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
            <h4>Data Ownership ({dataNodes.length})</h4>
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
        </>
      )}

      {show('connections') && (
        <>
          <div className="insp-section">
            <h4>
              Dependencies ({inspection.dependencies.upstream.length} upstream ·{' '}
              {inspection.dependencies.downstream.length} downstream)
            </h4>
            <div className="insp-list">
              {depCount === 0 ? (
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
            <h4>Interfaces ({inspection.interfaces.length})</h4>
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
        </>
      )}

      {show('evidence') && (
        <>
          <div className="insp-section">
            <h4>Evidence &amp; Claims ({inspection.evidence.length})</h4>
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
            <h4>Assumptions ({inspection.assumptions.length})</h4>
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
            <h4>Failure Modes ({inspection.failure_modes.length})</h4>
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
        </>
      )}

      {show('alternatives') && (
        <div className="insp-section">
          <h4>Alternatives ({inspection.alternatives.length})</h4>
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
      )}

      {tab === 'learn' && learning && (
        <LearningPanel api={learning.api} modelId={learning.modelId} nodeId={inspection.node.id} version={learning.version} />
      )}
    </aside>
  );
}
