import { useCallback, useEffect, useMemo, useState } from 'react';
import { CounterfactualView } from './components/CounterfactualView';
import { InspectorDrawer } from './components/InspectorDrawer';
import { LayerSidebar } from './components/LayerSidebar';
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

export function App() {
  const [activeTab, setActiveTab] = useState<WorkspaceTab>('xray');
  const [baseUrl, setBaseUrl] = useState(() => localStorage.getItem(STORAGE_KEYS.baseUrl) ?? '');
  const [token, setToken] = useState(() => localStorage.getItem(STORAGE_KEYS.token) ?? '');
  const [modelId, setModelId] = useState(() => localStorage.getItem(STORAGE_KEYS.modelId) ?? '');
  const [version, setVersion] = useState('current');

  const [projects, setProjects] = useState<ProjectSummary[]>([]);
  const [selectedProjectId, setSelectedProjectId] = useState('');
  const [versions, setVersions] = useState<GraphVersion[]>([]);

  const [baseView, setBaseView] = useState<XRayView | null>(null);
  const [drillDown, setDrillDown] = useState<XRayDrillDown | null>(null);
  const [loadingModel, setLoadingModel] = useState(false);
  const [statusMessage, setStatusMessage] = useState('');

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
        setStatusMessage('Model ID is required.');
        return;
      }
      setLoadingModel(true);
      setStatusMessage('Loading architecture model…');
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
        setStatusMessage(`Loaded v${view.version} (${view.total_nodes} nodes, ${view.total_edges} edges).`);
        try {
          const cf = await api.getAlternatives(id);
          setCounterfactualSet(cf);
        } catch {
          setCounterfactualSet(null);
        }
      } catch (err) {
        setStatusMessage(`Error: ${err instanceof Error ? err.message : String(err)}`);
      } finally {
        setLoadingModel(false);
      }
    },
    [api],
  );

  const handleFetchProjects = useCallback(async () => {
    setLoadingModel(true);
    setStatusMessage('Fetching projects…');
    try {
      const list = await api.listProjects();
      setProjects(list);
      setStatusMessage(`Found ${list.length} project${list.length === 1 ? '' : 's'}.`);
    } catch (err) {
      setStatusMessage(`Error: ${err instanceof Error ? err.message : String(err)}`);
    } finally {
      setLoadingModel(false);
    }
  }, [api]);

  const handleSelectProject = useCallback(
    async (projectId: string) => {
      setSelectedProjectId(projectId);
      if (!projectId) return;
      setLoadingModel(true);
      setStatusMessage('Resolving project architecture model…');
      try {
        const model = await api.getModelForProject(projectId);
        setModelId(model.id);
        setVersion('current');
        await loadModel(model.id, 'current');
      } catch (err) {
        setStatusMessage(`Error: ${err instanceof Error ? err.message : String(err)}`);
        setLoadingModel(false);
      }
    },
    [api, loadModel],
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
      } catch (err) {
        setStatusMessage(`Drill-down error: ${err instanceof Error ? err.message : String(err)}`);
      }
    },
    [api, modelId, version],
  );

  const handleGenerateAlternatives = useCallback(async () => {
    if (!modelId.trim()) return;
    setCfGenerating(true);
    setCfError(null);
    setDiffResult(null);
    try {
      const cf = await api.generateAlternatives(modelId.trim(), weights);
      setCounterfactualSet(cf);
      // Refresh versions list since new graph versions were materialized.
      const vers = await api.listVersions(modelId.trim()).catch(() => versions);
      setVersions(vers);
    } catch (err) {
      setCfError(err instanceof Error ? err.message : String(err));
    } finally {
      setCfGenerating(false);
    }
  }, [api, modelId, versions, weights]);

  const handleDiffAlternatives = useCallback(
    async (fromId: string, toId: string) => {
      if (!modelId.trim()) return;
      setDiffLoading(true);
      setCfError(null);
      try {
        const res = await api.diffAlternatives(modelId.trim(), fromId, toId);
        setDiffResult(res);
      } catch (err) {
        setCfError(err instanceof Error ? err.message : String(err));
      } finally {
        setDiffLoading(false);
      }
    },
    [api, modelId],
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
        projects={projects}
        selectedProjectId={selectedProjectId}
        versions={versions}
        activeTab={activeTab}
        loading={loadingModel}
        statusMessage={statusMessage}
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
                  By default, <code>/v1/*</code> requests proxy to <code>http://127.0.0.1:8080</code>{' '}
                  (configurable via <code>API_URL</code> or the API Base URL field).
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
          onGenerate={() => void handleGenerateAlternatives()}
          onDiff={(a, b) => void handleDiffAlternatives(a, b)}
        />
      )}
    </div>
  );
}
