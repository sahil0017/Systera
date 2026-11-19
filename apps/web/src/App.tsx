import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { CounterfactualView } from './components/CounterfactualView';
import { InspectorDrawer } from './components/InspectorDrawer';
import { LayerSidebar } from './components/LayerSidebar';
import { ToastStack, type ToastMessage } from './components/Toast';
import { TopBar, type WorkspaceTab } from './components/TopBar';
import { XRayCanvas } from './components/XRayCanvas';
import { SysteraApiClient } from './lib/api';
import { DEFAULT_CONSTRAINT_WEIGHTS } from './lib/constraints';
import { XRAY_LAYERS } from './lib/layers';
import type {
  AlternativeDiff,
  ComponentInspection,
  ConstraintId,
  CounterfactualSet,
  Direction,
  EvidenceState,
  GraphVersion,
  ProjectSummary,
  XRayDrillDown,
  XRayLayerId,
  XRayView,
} from './types/xray';

const STORAGE_KEYS = {
  baseUrl: 'systera.apiBaseUrl',
  token: 'systera.apiToken',
  modelId: 'systera.modelId',
};

function initialModelId(): string {
  const fromUrl = new URLSearchParams(window.location.search).get('model');
  return fromUrl || localStorage.getItem(STORAGE_KEYS.modelId) || '';
}

export function App() {
  const [activeTab, setActiveTab] = useState<WorkspaceTab>('xray');
  const [baseUrl, setBaseUrl] = useState(() => localStorage.getItem(STORAGE_KEYS.baseUrl) ?? '');
  const [token, setToken] = useState(() => localStorage.getItem(STORAGE_KEYS.token) ?? '');
  const [modelId, setModelId] = useState(initialModelId);
  const [version, setVersion] = useState('current');
  const [connectOpen, setConnectOpen] = useState(() => !(localStorage.getItem(STORAGE_KEYS.token) && initialModelId()));

  const [projects, setProjects] = useState<ProjectSummary[]>([]);
  const [selectedProjectId, setSelectedProjectId] = useState('');
  const [versions, setVersions] = useState<GraphVersion[]>([]);

  const [baseView, setBaseView] = useState<XRayView | null>(null);
  const [drillDown, setDrillDown] = useState<XRayDrillDown | null>(null);
  const [loadingModel, setLoadingModel] = useState(false);

  const [visibleLayers, setVisibleLayers] = useState<Set<XRayLayerId>>(
    () => new Set(XRAY_LAYERS.map((l) => l.id)),
  );
  const [search, setSearch] = useState('');
  const [evidenceFilter, setEvidenceFilter] = useState<'ALL' | EvidenceState>('ALL');

  const [selectedNodeId, setSelectedNodeId] = useState<string | null>(null);
  const [inspection, setInspection] = useState<ComponentInspection | null>(null);
  const [inspLoading, setInspLoading] = useState(false);
  const [inspError, setInspError] = useState<string | null>(null);

  const [weights, setWeights] = useState<Record<ConstraintId, number>>(DEFAULT_CONSTRAINT_WEIGHTS);
  const [counterfactualSet, setCounterfactualSet] = useState<CounterfactualSet | null>(null);
  const [cfGenerating, setCfGenerating] = useState(false);
  const [cfError, setCfError] = useState<string | null>(null);
  const [diffResult, setDiffResult] = useState<AlternativeDiff | null>(null);
  const [diffLoading, setDiffLoading] = useState(false);

  const [toasts, setToasts] = useState<ToastMessage[]>([]);
  const toastSeq = useRef(1);
  const autoLoaded = useRef(false);

  const pushToast = useCallback((kind: ToastMessage['kind'], text: string) => {
    const id = toastSeq.current++;
    setToasts((prev) => [...prev.slice(-3), { id, kind, text }]);
    window.setTimeout(() => {
      setToasts((prev) => prev.filter((t) => t.id !== id));
    }, 3800);
  }, []);

  const api = useMemo(() => new SysteraApiClient({ baseUrl, token }), [baseUrl, token]);

  useEffect(() => {
    localStorage.setItem(STORAGE_KEYS.baseUrl, baseUrl);
  }, [baseUrl]);
  useEffect(() => {
    localStorage.setItem(STORAGE_KEYS.token, token);
  }, [token]);
  useEffect(() => {
    localStorage.setItem(STORAGE_KEYS.modelId, modelId);
  }, [modelId]);

  const loadModel = useCallback(
    async (targetModelId: string, targetVersion: string) => {
      const id = targetModelId.trim();
      if (!id) {
        pushToast('error', 'Model ID is required.');
        return;
      }
      setLoadingModel(true);
      setCfError(null);
      setDiffResult(null);
      try {
        const [view, vers] = await Promise.all([
          api.getXRay(id, { version: targetVersion || 'current' }),
          api.listVersions(id).catch(() => [] as GraphVersion[]),
        ]);
        setBaseView(view);
        setVersions(vers);
        setDrillDown(null);
        setSelectedNodeId(null);
        setInspection(null);
        setConnectOpen(false);
        pushToast('success', `Loaded v${view.version} (${view.total_nodes} nodes, ${view.total_edges} edges)`);
        try {
          const cf = await api.getAlternatives(id);
          setCounterfactualSet(cf);
        } catch {
          setCounterfactualSet(null);
        }
      } catch (err) {
        pushToast('error', err instanceof Error ? err.message : String(err));
      } finally {
        setLoadingModel(false);
      }
    },
    [api, pushToast],
  );

  // Auto-bootstrap workspace token in background if none saved, then auto-load ?model= or latest project.
  useEffect(() => {
    if (autoLoaded.current) return;
    autoLoaded.current = true;
    void (async () => {
      let activeToken = token.trim();
      if (!activeToken) {
        try {
          const res = await fetch(`${baseUrl.replace(/\/$/, '')}/v1/auth/workspace-bootstrap`, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
          });
          if (res.ok) {
            const data = (await res.json()) as { access_token?: string };
            if (data.access_token) {
              activeToken = data.access_token;
              setToken(activeToken);
            }
          }
        } catch {
          // manual connect fallback
        }
      }
      if (!activeToken) return;

      const client = new SysteraApiClient({ baseUrl, token: activeToken });
      let targetModel = modelId.trim();
      if (!targetModel) {
        try {
          const projs = await client.listProjects();
          setProjects(projs);
          if (projs[0]) {
            setSelectedProjectId(projs[0].id);
            const m = await client.getModelForProject(projs[0].id);
            targetModel = m.id;
            setModelId(targetModel);
          }
        } catch {
          // ignore
        }
      }
      if (targetModel) {
        setLoadingModel(true);
        try {
          const [view, vers] = await Promise.all([
            client.getXRay(targetModel, { version: 'current' }),
            client.listVersions(targetModel).catch(() => [] as GraphVersion[]),
          ]);
          setBaseView(view);
          setVersions(vers);
          setConnectOpen(false);
          pushToast('success', `Loaded v${view.version} (${view.total_nodes} nodes, ${view.total_edges} edges)`);
          try {
            const cf = await client.getAlternatives(targetModel);
            setCounterfactualSet(cf);
          } catch {
            setCounterfactualSet(null);
          }
        } catch (err) {
          pushToast('error', err instanceof Error ? err.message : String(err));
        } finally {
          setLoadingModel(false);
        }
      }
    })();
  }, [baseUrl, modelId, pushToast, token]);

  // Keyboard shortcuts: Escape exits drill-down/selection, "/" focuses search.
  useEffect(() => {
    const onKeyDown = (e: KeyboardEvent) => {
      const tag = (e.target as HTMLElement | null)?.tagName;
      if (e.key === 'Escape') {
        if (drillDown) setDrillDown(null);
        else if (selectedNodeId) {
          setSelectedNodeId(null);
          setInspection(null);
        }
      } else if (e.key === '/' && tag !== 'INPUT' && tag !== 'TEXTAREA' && tag !== 'SELECT') {
        e.preventDefault();
        document.querySelector<HTMLInputElement>('.search-input')?.focus();
      }
    };
    window.addEventListener('keydown', onKeyDown);
    return () => window.removeEventListener('keydown', onKeyDown);
  }, [drillDown, selectedNodeId]);

  const handleFetchProjects = useCallback(async () => {
    setLoadingModel(true);
    try {
      const list = await api.listProjects();
      setProjects(list);
      pushToast('info', `Found ${list.length} project${list.length === 1 ? '' : 's'}`);
    } catch (err) {
      pushToast('error', err instanceof Error ? err.message : String(err));
    } finally {
      setLoadingModel(false);
    }
  }, [api, pushToast]);

  const handleSelectProject = useCallback(
    async (projectId: string) => {
      setSelectedProjectId(projectId);
      if (!projectId) return;
      setLoadingModel(true);
      try {
        const model = await api.getModelForProject(projectId);
        setModelId(model.id);
        setVersion('current');
        await loadModel(model.id, 'current');
      } catch (err) {
        pushToast('error', err instanceof Error ? err.message : String(err));
        setLoadingModel(false);
      }
    },
    [api, loadModel, pushToast],
  );

  const selectNode = useCallback(
    async (nodeId: string) => {
      if (!modelId.trim()) return;
      setSelectedNodeId(nodeId);
      setInspLoading(true);
      setInspError(null);
      try {
        const res = await api.inspectComponent(modelId.trim(), nodeId, { version });
        setInspection(res);
      } catch (err) {
        setInspError(err instanceof Error ? err.message : String(err));
      } finally {
        setInspLoading(false);
      }
    },
    [api, modelId, version],
  );

  const runDrillDown = useCallback(
    async (nodeId: string, depth = 2, direction: Direction = 'both') => {
      if (!modelId.trim()) return;
      try {
        const res = await api.getDrillDown(modelId.trim(), nodeId, { version, depth, direction });
        setDrillDown(res);
        setVisibleLayers(new Set(XRAY_LAYERS.map((l) => l.id)));
        pushToast('info', `Drill-down active (${depth} hop${depth > 1 ? 's' : ''}, ${direction})`);
      } catch (err) {
        pushToast('error', err instanceof Error ? err.message : String(err));
      }
    },
    [api, modelId, pushToast, version],
  );

  const handleGenerateAlternatives = useCallback(async () => {
    if (!modelId.trim()) return;
    setCfGenerating(true);
    setCfError(null);
    setDiffResult(null);
    try {
      const cf = await api.generateAlternatives(modelId.trim(), weights);
      setCounterfactualSet(cf);
      const vers = await api.listVersions(modelId.trim()).catch(() => versions);
      setVersions(vers);
      pushToast('success', `Generated ${cf.alternatives.length} alternatives + recorded ADR`);
    } catch (err) {
      const msg = err instanceof Error ? err.message : String(err);
      setCfError(msg);
      pushToast('error', msg);
    } finally {
      setCfGenerating(false);
    }
  }, [api, modelId, pushToast, versions, weights]);

  const handleDiffAlternatives = useCallback(
    async (fromId: string, toId: string) => {
      if (!modelId.trim()) return;
      setDiffLoading(true);
      setCfError(null);
      try {
        const res = await api.diffAlternatives(modelId.trim(), fromId, toId);
        setDiffResult(res);
        pushToast('info', `Diffed v${res.graph_diff.from_version} → v${res.graph_diff.to_version}`);
      } catch (err) {
        setCfError(err instanceof Error ? err.message : String(err));
      } finally {
        setDiffLoading(false);
      }
    },
    [api, modelId, pushToast],
  );

  const handleCopy = useCallback(
    (text: string, label: string) => {
      void navigator.clipboard?.writeText(text);
      pushToast('info', `Copied ${label}`);
    },
    [pushToast],
  );

  const activeView = drillDown ?? baseView;

  return (
    <div className="app-shell">
      <TopBar
        baseUrl={baseUrl}
        token={token}
        modelId={modelId}
        version={version}
        loadedVersion={baseView?.version ?? null}
        nodeCount={baseView?.total_nodes ?? 0}
        edgeCount={baseView?.total_edges ?? 0}
        projects={projects}
        selectedProjectId={selectedProjectId}
        versions={versions}
        activeTab={activeTab}
        loading={loadingModel}
        connectOpen={connectOpen}
        onToggleConnect={() => setConnectOpen((v) => !v)}
        onBaseUrlChange={setBaseUrl}
        onTokenChange={setToken}
        onModelIdChange={setModelId}
        onVersionChange={(v) => {
          setVersion(v);
          if (modelId.trim()) void loadModel(modelId, v);
        }}
        onSelectProject={(pid) => void handleSelectProject(pid)}
        onFetchProjects={() => void handleFetchProjects()}
        onLoadModel={() => void loadModel(modelId, version)}
        onTabChange={setActiveTab}
        onReset={() => {
          setDrillDown(null);
          setVisibleLayers(new Set(XRAY_LAYERS.map((l) => l.id)));
          setSearch('');
          setEvidenceFilter('ALL');
          setWeights(DEFAULT_CONSTRAINT_WEIGHTS);
          pushToast('info', 'Reset filters and drill-down');
        }}
      />

      {activeTab === 'xray' ? (
        <main className="workspace">
          <LayerSidebar
            layers={activeView?.layers ?? []}
            visibleLayers={visibleLayers}
            onToggleLayer={(id) =>
              setVisibleLayers((prev) => {
                const next = new Set(prev);
                if (next.has(id)) next.delete(id);
                else next.add(id);
                return next;
              })
            }
            onSoloLayer={(id) => setVisibleLayers(new Set([id]))}
            onShowAllLayers={() => setVisibleLayers(new Set(XRAY_LAYERS.map((l) => l.id)))}
            search={search}
            onSearchChange={setSearch}
            evidenceFilter={evidenceFilter}
            onEvidenceFilterChange={setEvidenceFilter}
          />

          {activeView ? (
            <XRayCanvas
              view={activeView}
              drillDown={drillDown}
              visibleLayers={visibleLayers}
              search={search}
              evidenceFilter={evidenceFilter}
              selectedNodeId={selectedNodeId}
              onSelectNode={(id) => void selectNode(id)}
              onQuickDrillDown={(id) => void runDrillDown(id, 2, 'both')}
              onChangeDrillDown={(d, dir) => {
                if (drillDown) void runDrillDown(drillDown.center_node_id, d, dir);
              }}
              onExitDrillDown={() => setDrillDown(null)}
            />
          ) : (
            <section className="canvas-area">
              <div className="empty-state">
                <h3>Connect to Systera Control Plane</h3>
                <p>
                  Enter your RS256 Bearer token in the connection bar above, then click{' '}
                  <strong>Fetch Projects</strong> or paste an Architecture Model UUID and click{' '}
                  <strong>Load Model</strong>.
                </p>
                <p className="insp-sub">
                  Tip: pass <code>?model=&lt;uuid&gt;</code> in the URL to open a specific model directly.
                </p>
              </div>
            </section>
          )}

          <InspectorDrawer
            inspection={inspection}
            loading={inspLoading}
            error={inspError}
            onSelectNode={(id) => void selectNode(id)}
            onDrillDown={(id) => void runDrillDown(id, 2, 'both')}
            onCopy={handleCopy}
            learning={modelId.trim() ? { api, modelId: modelId.trim(), version } : undefined}
          />
        </main>
      ) : (
        <CounterfactualView
          modelId={modelId.trim()}
          set={counterfactualSet}
          weights={weights}
          generating={cfGenerating}
          diffResult={diffResult}
          diffLoading={diffLoading}
          error={cfError}
          onWeightChange={(c, val) => setWeights((prev) => ({ ...prev, [c]: val }))}
          onApplyPreset={(presetWeights, label) => {
            setWeights(presetWeights);
            pushToast('info', `Applied "${label}" constraint weights`);
          }}
          onGenerate={() => void handleGenerateAlternatives()}
          onDiff={(a, b) => void handleDiffAlternatives(a, b)}
        />
      )}

      <ToastStack
        toasts={toasts}
        onDismiss={(id) => setToasts((prev) => prev.filter((t) => t.id !== id))}
      />
    </div>
  );
}
