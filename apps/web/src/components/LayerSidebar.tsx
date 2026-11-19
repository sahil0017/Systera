import { XRAY_LAYERS } from '../lib/layers';
import type { EvidenceState, XRayLayerGroup, XRayLayerId } from '../types/xray';

interface LayerSidebarProps {
  layers: XRayLayerGroup[];
  visibleLayers: Set<XRayLayerId>;
  onToggleLayer: (id: XRayLayerId) => void;
  onSoloLayer: (id: XRayLayerId) => void;
  onShowAllLayers: () => void;
  search: string;
  onSearchChange: (val: string) => void;
  evidenceFilter: 'ALL' | EvidenceState;
  onEvidenceFilterChange: (val: 'ALL' | EvidenceState) => void;
}

const EVIDENCE_OPTIONS: Array<'ALL' | EvidenceState> = ['ALL', 'OBSERVED', 'INFERRED', 'PROPOSED'];

export function LayerSidebar({
  layers,
  visibleLayers,
  onToggleLayer,
  onSoloLayer,
  onShowAllLayers,
  search,
  onSearchChange,
  evidenceFilter,
  onEvidenceFilterChange,
}: LayerSidebarProps) {
  const countByLayer = Object.fromEntries(layers.map((g) => [g.id, g.node_count])) as Partial<
    Record<XRayLayerId, number>
  >;

  return (
    <aside className="sidebar">
      <section className="panel">
        <div className="panel-header">
          <h2>X-Ray Layers (8)</h2>
          <button type="button" className="link-btn" onClick={onShowAllLayers}>
            Show All
          </button>
        </div>
        <div className="layer-nav">
          {XRAY_LAYERS.map((layer) => (
            <div className="layer-toggle" key={layer.id}>
              <label>
                <input
                  type="checkbox"
                  checked={visibleLayers.has(layer.id)}
                  onChange={() => onToggleLayer(layer.id)}
                />
                <span className="layer-swatch" style={{ background: layer.color }} />
                <span>{layer.label}</span>
              </label>
              <span className="layer-count">{countByLayer[layer.id] ?? 0}</span>
              <button
                type="button"
                className="solo-btn"
                title="Focus only this layer"
                onClick={() => onSoloLayer(layer.id)}
              >
                Solo
              </button>
            </div>
          ))}
        </div>
      </section>

      <section className="panel">
        <h2>Filter</h2>
        <input
          className="search-input"
          type="search"
          value={search}
          onChange={(e) => onSearchChange(e.target.value)}
          placeholder="Search components or keys..."
        />
        <div className="pill-group">
          {EVIDENCE_OPTIONS.map((ev) => (
            <button
              key={ev}
              type="button"
              className={`pill ${evidenceFilter === ev ? 'active' : ''}`}
              onClick={() => onEvidenceFilterChange(ev)}
            >
              {ev === 'ALL' ? 'All' : ev.charAt(0) + ev.slice(1).toLowerCase()}
            </button>
          ))}
        </div>
      </section>

      <section className="panel legend">
        <h2>Evidence Classes</h2>
        <div>
          <span className="badge ev-OBSERVED">OBSERVED</span> Direct evidence
        </div>
        <div>
          <span className="badge ev-INFERRED">INFERRED</span> Derived from evidence
        </div>
        <div>
          <span className="badge ev-PROPOSED">PROPOSED</span> Architectural proposal
        </div>
      </section>
    </aside>
  );
}
