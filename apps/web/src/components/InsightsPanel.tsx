import { LAYER_BY_ID } from '../lib/layers';
import type { Hotspot, ModelInsights } from '../lib/insights';

interface Props {
  insights: ModelInsights;
  onSelectNode: (id: string) => void;
}

function Spots({ title, items, empty, onSelect }: { title: string; items: Hotspot[]; empty: string; onSelect: (id: string) => void }) {
  return (
    <div className="insp-section">
      <h4>{title}</h4>
      {items.length === 0 ? (
        <div className="empty-note">{empty}</div>
      ) : (
        items.map((h) => (
          <button key={h.node.id} type="button" className="spot" onClick={() => onSelect(h.node.id)}>
            <span className="layer-swatch" style={{ background: LAYER_BY_ID[h.node.layer]?.color }} />
            <span className="spot-name">{h.node.name}</span>
            <span className="spot-meta">{h.detail}</span>
          </button>
        ))
      )}
    </div>
  );
}

/** Shown in the inspector column when nothing is selected: where to look first. */
export function InsightsPanel({ insights: i, onSelectNode }: Props) {
  const total = Math.max(1, i.nodes);
  return (
    <aside className="inspector">
      <div className="inspector-header">
        <div className="node-type">Model insights</div>
        <h3 className="insight-headline">{i.headline}</h3>
        <div className="insp-sub">Computed from this graph version. Click a component to inspect it, or press <kbd>⌘K</kbd> to search.</div>
      </div>

      <div className="insp-section">
        <h4>Evidence coverage · {i.score}%</h4>
        <div className="ev-bar" role="img" aria-label={`${i.evidence.OBSERVED} observed, ${i.evidence.INFERRED} inferred, ${i.evidence.PROPOSED} proposed`}>
          <span className="ev-OBSERVED" style={{ width: `${(i.evidence.OBSERVED / total) * 100}%` }} />
          <span className="ev-INFERRED" style={{ width: `${(i.evidence.INFERRED / total) * 100}%` }} />
          <span className="ev-PROPOSED" style={{ width: `${(i.evidence.PROPOSED / total) * 100}%` }} />
        </div>
        <div className="ev-legend">
          <span><i className="ev-OBSERVED" />{i.evidence.OBSERVED} observed</span>
          <span><i className="ev-INFERRED" />{i.evidence.INFERRED} inferred</span>
          <span><i className="ev-PROPOSED" />{i.evidence.PROPOSED} proposed</span>
        </div>
      </div>

      <Spots title="Likely single points of failure" items={i.singlePoints} onSelect={onSelectNode}
        empty="None: every component with several dependents has a replica or failover recorded." />
      <Spots title="Most depended on" items={i.mostDependedOn} onSelect={onSelectNode} empty="No component has more than one dependent yet." />

      {i.syncChain && (
        <div className="insp-section">
          <h4>Longest synchronous chain · {i.syncChain.length} hops</h4>
          <div className="chain">
            {i.syncChain.nodes.map((n, idx) => (
              <span key={n.id} className="chain-step">
                <button type="button" className="link-btn" onClick={() => onSelectNode(n.id)}>{n.name}</button>
                {idx < i.syncChain!.nodes.length - 1 && <span className="chain-arrow">→</span>}
              </span>
            ))}
          </div>
          <div className="insp-sub">Each synchronous hop adds latency and couples availability. Consider async boundaries where a caller doesn't need an immediate answer.</div>
        </div>
      )}

      {i.orphans.length > 0 && (
        <div className="insp-section">
          <h4>Unconnected components</h4>
          <div className="chip-wrap">
            {i.orphans.map((n) => <button key={n.id} type="button" className="chip" onClick={() => onSelectNode(n.id)}>{n.name}</button>)}
          </div>
        </div>
      )}
      <div className="insp-sub">Heuristics over the graph to guide where to look, not verdicts about the design.</div>
    </aside>
  );
}
