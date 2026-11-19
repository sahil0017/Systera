import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react';
import { LAYER_BY_ID } from '../lib/layers';
import type { Direction, EvidenceState, XRayDrillDown, XRayEdge, XRayLayerId, XRayNode, XRayView } from '../types/xray';

interface XRayCanvasProps {
  view: XRayView | XRayDrillDown;
  drillDown: XRayDrillDown | null;
  visibleLayers: Set<XRayLayerId>;
  search: string;
  evidenceFilter: 'ALL' | EvidenceState;
  selectedNodeId: string | null;
  onSelectNode: (id: string) => void;
  onQuickDrillDown: (id: string) => void;
  onChangeDrillDown: (depth: number, direction: Direction) => void;
  onExitDrillDown: () => void;
}

interface EdgePath {
  id: string;
  d: string;
  label: string;
  labelX: number;
  labelY: number;
  highlighted: boolean;
}

export function XRayCanvas({
  view,
  drillDown,
  visibleLayers,
  search,
  evidenceFilter,
  selectedNodeId,
  onSelectNode,
  onQuickDrillDown,
  onChangeDrillDown,
  onExitDrillDown,
}: XRayCanvasProps) {
  const stageRef = useRef<HTMLDivElement | null>(null);
  const cardRefs = useRef<Map<string, HTMLDivElement>>(new Map());
  const [svgSize, setSvgSize] = useState({ width: 0, height: 0 });
  const [edgePaths, setEdgePaths] = useState<EdgePath[]>([]);
  const [hoveredNodeId, setHoveredNodeId] = useState<string | null>(null);

  const focusNodeId = hoveredNodeId ?? selectedNodeId;

  const allEdges = useMemo<XRayEdge[]>(() => {
    const out: XRayEdge[] = [...view.cross_layer_edges];
    for (const g of view.layers) {
      for (const e of g.edges) out.push(e);
    }
    return out;
  }, [view]);

  const connectedToFocus = useMemo(() => {
    if (!focusNodeId) return null;
    const set = new Set<string>([focusNodeId]);
    for (const e of allEdges) {
      if (e.source_node_id === focusNodeId) set.add(e.target_node_id);
      if (e.target_node_id === focusNodeId) set.add(e.source_node_id);
    }
    return set;
  }, [allEdges, focusNodeId]);

  const matchesFilter = useCallback(
    (n: XRayNode) => {
      if (evidenceFilter !== 'ALL' && n.evidence_state !== evidenceFilter) return false;
      if (search) {
        const q = search.toLowerCase();
        return (
          n.name.toLowerCase().includes(q) ||
          n.key.toLowerCase().includes(q) ||
          n.type.toLowerCase().includes(q)
        );
      }
      return true;
    },
    [evidenceFilter, search],
  );

  const recomputeEdges = useCallback(() => {
    const stage = stageRef.current;
    if (!stage) return;
    const stageRect = stage.getBoundingClientRect();
    const positions = new Map<string, { cx: number; cy: number }>();

    cardRefs.current.forEach((el, id) => {
      const r = el.getBoundingClientRect();
      positions.set(id, {
        cx: r.left - stageRect.left + r.width / 2,
        cy: r.top - stageRect.top + r.height / 2,
      });
    });

    const nextPaths: EdgePath[] = [];
    for (const e of allEdges) {
      const a = positions.get(e.source_node_id);
      const b = positions.get(e.target_node_id);
      if (!a || !b) continue;
      const midY = (a.cy + b.cy) / 2;
      const highlighted = Boolean(
        focusNodeId && (e.source_node_id === focusNodeId || e.target_node_id === focusNodeId),
      );
      nextPaths.push({
        id: e.id,
        d: `M ${a.cx} ${a.cy} C ${a.cx} ${midY}, ${b.cx} ${midY}, ${b.cx} ${b.cy}`,
        label: e.relation_type,
        labelX: (a.cx + b.cx) / 2,
        labelY: midY,
        highlighted,
      });
    }
    setSvgSize({ width: stageRect.width, height: stageRect.height });
    setEdgePaths(nextPaths);
  }, [allEdges, focusNodeId]);

  useLayoutEffect(() => {
    recomputeEdges();
  }, [recomputeEdges, visibleLayers, search, evidenceFilter]);

  useEffect(() => {
    const stage = stageRef.current;
    if (!stage) return;
    const observer = new ResizeObserver(() => recomputeEdges());
    observer.observe(stage);
    window.addEventListener('resize', recomputeEdges);
    return () => {
      observer.disconnect();
      window.removeEventListener('resize', recomputeEdges);
    };
  }, [recomputeEdges]);

  const visibleGroups = view.layers.filter((g) => visibleLayers.has(g.id));

  return (
    <section className="canvas-area">
      {drillDown && (
        <div className="drilldown-bar">
          <div>
            <strong>Drill-Down Active:</strong> {drillDown.center_layer.replaceAll('_', ' ')} ·{' '}
            {drillDown.depth} hop{drillDown.depth > 1 ? 's' : ''} ({drillDown.direction})
          </div>
          <div className="drilldown-controls">
            <label>
              Depth{' '}
              <select
                value={drillDown.depth}
                onChange={(e) => onChangeDrillDown(Number(e.target.value), drillDown.direction)}
              >
                <option value={1}>1 hop</option>
                <option value={2}>2 hops</option>
                <option value={3}>3 hops</option>
              </select>
            </label>
            <label>
              Direction{' '}
              <select
                value={drillDown.direction}
                onChange={(e) => onChangeDrillDown(drillDown.depth, e.target.value as Direction)}
              >
                <option value="both">Both</option>
                <option value="out">Downstream</option>
                <option value="in">Upstream</option>
              </select>
            </label>
            <button type="button" className="btn btn-secondary" onClick={onExitDrillDown}>
              Exit Drill-Down (Esc)
            </button>
          </div>
        </div>
      )}

      <div ref={stageRef} className="xray-stage">
        <svg className="edge-overlay" viewBox={`0 0 ${svgSize.width} ${svgSize.height}`}>
          <defs>
            <marker id="arrow-active" viewBox="0 0 10 10" refX="8" refY="5" markerWidth="6" markerHeight="6" orient="auto-start-reverse">
              <path d="M 0 1 L 10 5 L 0 9 z" fill="#38bdf8" />
            </marker>
          </defs>
          {edgePaths.map((p) => (
            <g key={p.id}>
              <path
                d={p.d}
                fill="none"
                stroke={p.highlighted ? '#38bdf8' : 'rgba(148, 163, 184, 0.2)'}
                strokeWidth={p.highlighted ? 2.4 : 1}
                strokeDasharray={p.highlighted ? undefined : 'none'}
                markerEnd={p.highlighted ? 'url(#arrow-active)' : undefined}
              />
              {p.highlighted && (
                <text
                  x={p.labelX}
                  y={p.labelY - 4}
                  textAnchor="middle"
                  className="edge-label"
                >
                  {p.label}
                </text>
              )}
            </g>
          ))}
        </svg>

        <div className="layer-stack">
          {visibleGroups.map((group) => {
            const meta = LAYER_BY_ID[group.id];
            const filteredNodes = group.nodes.filter(matchesFilter);
            return (
              <div
                key={group.id}
                className="layer-band"
                style={{ '--layer-color': meta.color } as React.CSSProperties}
              >
                <div className="layer-band-header">
                  <div className="layer-band-title">
                    <span className="layer-swatch" style={{ background: meta.color }} />
                    {group.order}. {group.label}
                  </div>
                  <div className="layer-band-meta">{filteredNodes.length} components</div>
                </div>
                <div className="node-grid">
                  {filteredNodes.length === 0 ? (
                    <div className="empty-note">No components in this layer match the current filter.</div>
                  ) : (
                    filteredNodes.map((n) => {
                      const isSelected = n.id === selectedNodeId;
                      const isCenter = drillDown?.center_node_id === n.id;
                      const isDimmed = Boolean(connectedToFocus && !connectedToFocus.has(n.id));
                      return (
                        <div
                          key={n.id}
                          ref={(el) => {
                            if (el) cardRefs.current.set(n.id, el);
                            else cardRefs.current.delete(n.id);
                          }}
                          className={`node-card ${isSelected ? 'selected' : ''} ${isCenter ? 'center' : ''} ${isDimmed ? 'dimmed' : ''}`}
                          onClick={() => onSelectNode(n.id)}
                          onMouseEnter={() => setHoveredNodeId(n.id)}
                          onMouseLeave={() => setHoveredNodeId(null)}
                        >
                          <div className="node-card-top">
                            <span className="node-type">{n.type}</span>
                            <span className={`badge ev-${n.evidence_state}`}>{n.evidence_state}</span>
                          </div>
                          <div className="node-name">{n.name}</div>
                          <div className="node-key">{n.key}</div>
                          <div className="node-footer">
                            {drillDown && typeof n.depth === 'number' ? (
                              <span className="node-depth">
                                {n.depth === 0 ? 'Center' : `${n.depth} hop${n.depth > 1 ? 's' : ''}`}
                              </span>
                            ) : (
                              <span />
                            )}
                            <button
                              type="button"
                              className="micro-btn"
                              title="Drill down neighbourhood from this component"
                              onClick={(e) => {
                                e.stopPropagation();
                                onQuickDrillDown(n.id);
                              }}
                            >
                              Drill ↓
                            </button>
                          </div>
                        </div>
                      );
                    })
                  )}
                </div>
              </div>
            );
          })}
        </div>
      </div>
    </section>
  );
}
