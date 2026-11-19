import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { CommandPalette, type PaletteItem } from './components/CommandPalette';
import { ConnectScreen, type ServerStatus } from './components/ConnectScreen';
import { CounterfactualView } from './components/CounterfactualView';
import { Header, MODEL_TABS, type WorkspaceTab } from './components/Header';
import { InsightsPanel } from './components/InsightsPanel';
import { InspectorDrawer, type InspectorTab } from './components/InspectorDrawer';
import { LearnOverview } from './components/LearnOverview';
import { TwinView } from './components/TwinView';
import { LayerSidebar } from './components/LayerSidebar';
import { ResearchView, type ResearchSection } from './components/ResearchView';
import { ToastStack, type ToastMessage } from './components/Toast';
import { WebsitesView } from './components/WebsitesView';
import { XRayCanvas } from './components/XRayCanvas';
import { SysteraApiClient } from './lib/api';
import { DEFAULT_CONSTRAINT_WEIGHTS } from './lib/constraints';
import { computeInsights } from './lib/insights';
import { LAYER_BY_ID, XRAY_LAYERS } from './lib/layers';
import {
  askOtherTabsForToken, inspectToken, loadBaseUrl, serveTokenToOtherTabs, loadRecents, loadTheme, loadToken, readLink, rememberModel, saveBaseUrl, saveTheme, saveToken,
  writeLink, type RecentModel, type Theme,
} from './lib/session';
import type {
  AlternativeDiff, ComponentInspection, ConstraintId, CounterfactualSet, Direction, EvidenceState, GraphVersion,
  ProjectSummary, XRayDrillDown, XRayLayerId, XRayView,
} from './types/xray';

const allLayers = () => new Set(XRAY_LAYERS.map((l) => l.id));
const TABS: WorkspaceTab[] = ['xray', 'counterfactual', 'twin', 'learn', 'websites', 'research'];
const SECTIONS: ResearchSection[] = ['trends', 'reports', 'knowledge', 'memory', 'compare', 'feed', 'sources', 'provenance'];

export function App() {
  const link = useMemo(() => readLink(), []);
  const [tab, setTabState] = useState<WorkspaceTab>(TABS.includes(link.tab as WorkspaceTab) ? (link.tab as WorkspaceTab) : 'xray');
  const [siteId, setSiteId] = useState(link.site ?? '');
  const [researchSection, setResearchSection] = useState<ResearchSection>(SECTIONS.includes(link.section as ResearchSection) ? (link.section as ResearchSection) : 'trends');
  const [runId, setRunId] = useState(link.run ?? '');
  const [memoryProjectId, setMemoryProjectId] = useState('');
  const [inspTabReq, setInspTabReq] = useState<{ tab: InspectorTab; seq: number } | undefined>(undefined);
  const [baseUrl, setBaseUrl] = useState(loadBaseUrl);
  const initialToken = useMemo(() => {
    const t = loadToken();
    // A saved token that has expired is useless: drop it instead of showing it as an error.
    const info = inspectToken(t.token);
    return { ...t, token: info.expiresAt && !info.valid ? '' : t.token, expiredAt: info.expiresAt && !info.valid ? info.expiresAt : undefined };
  }, []);
  const [token, setToken] = useState(initialToken.token);
  const [handoff, setHandoff] = useState<'asking' | 'done'>(initialToken.token ? 'done' : 'asking');
  const [remember, setRemember] = useState(initialToken.remember);
  const [connected, setConnected] = useState(false);
  const [connecting, setConnecting] = useState(false);
  const [connectError, setConnectError] = useState<string | null>(null);
  const [serverStatus, setServerStatus] = useState<ServerStatus>('checking');
  const [theme, setTheme] = useState<Theme>(loadTheme);
  const [recents, setRecents] = useState<RecentModel[]>(loadRecents);
  const [picking, setPicking] = useState(false); // showing the project picker over a loaded model

  const [modelId, setModelId] = useState(link.model ?? '');
  const [projectName, setProjectName] = useState('');
  const [version, setVersion] = useState('current');
  const [projects, setProjects] = useState<ProjectSummary[] | null>(null);
  const [versions, setVersions] = useState<GraphVersion[]>([]);
  const [baseView, setBaseView] = useState<XRayView | null>(null);
  const [drillDown, setDrillDown] = useState<XRayDrillDown | null>(null);
  const [loadingModel, setLoadingModel] = useState(false);

  const [visibleLayers, setVisibleLayers] = useState<Set<XRayLayerId>>(allLayers);
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

  const [paletteOpen, setPaletteOpen] = useState(false);
  const [sidebarOpen, setSidebarOpen] = useState(() => typeof window === 'undefined' || window.innerWidth >= 1180);
  const [toasts, setToasts] = useState<ToastMessage[]>([]);
  const toastSeq = useRef(1);
  const pendingNode = useRef<string | null>(link.node ?? null);

  const tokenInfo = useMemo(() => inspectToken(token), [token]);
  const api = useMemo(() => new SysteraApiClient({ baseUrl, token }), [baseUrl, token]);

  const pushToast = useCallback((kind: ToastMessage['kind'], text: string) => {
    const id = toastSeq.current++;
    setToasts((prev) => [...prev.slice(-3), { id, kind, text }]);
    window.setTimeout(() => setToasts((prev) => prev.filter((t) => t.id !== id)), 3800);
  }, []);

  const setTab = useCallback((t: WorkspaceTab) => {
    setTabState(t);
    if (!MODEL_TABS.includes(t)) setPicking(false);
  }, []);

  // Theme: explicit choice or the OS setting.
  useEffect(() => {
    saveTheme(theme);
    const root = document.documentElement;
    if (theme === 'system') root.removeAttribute('data-theme');
    else root.setAttribute('data-theme', theme);
  }, [theme]);

  useEffect(() => saveBaseUrl(baseUrl), [baseUrl]);
  useEffect(() => saveToken(token, remember), [token, remember]);

  // No usable token in this tab (e.g. opened from Engineering Lens): borrow one from another
  // open, connected workspace tab in this browser, or auto-bootstrap from the server if enabled.
  useEffect(() => {
    if (handoff !== 'asking') return;
    let live = true;
    void askOtherTabsForToken().then(async (t) => {
      if (!live) return;
      if (t) {
        setToken(t);
        setHandoff('done');
        return;
      }
      try {
        const res = await fetch(`${baseUrl.replace(/\/$/, '')}/v1/auth/workspace-bootstrap`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
        });
        if (live && res.ok) {
          const data = (await res.json()) as { access_token?: string };
          if (data.access_token) {
            setRemember(true);
            setToken(data.access_token);
          }
        }
      } catch {
        // manual sign-in screen fallback
      }
      if (live) setHandoff('done');
    });
    return () => { live = false; };
  }, [baseUrl, handoff]);

  // While connected, answer other tabs that ask.
  const tokenRef = useRef(token);
  tokenRef.current = connected ? token : '';
  useEffect(() => serveTokenToOtherTabs(() => tokenRef.current), []);

  // Server reachability, re-checked when the URL changes.
  useEffect(() => {
    let live = true;
    setServerStatus('checking');
    const t = window.setTimeout(() => {
      void new SysteraApiClient({ baseUrl }).ready().then((s) => live && setServerStatus(s));
    }, 300);
    return () => { live = false; window.clearTimeout(t); };
  }, [baseUrl]);

  // Deep link follows what is on screen.
  useEffect(() => {
    if (!connected) return;
    const onModel = MODEL_TABS.includes(tab);
    writeLink({
      model: baseView ? modelId : undefined,
      node: onModel && tab === 'xray' ? selectedNodeId ?? undefined : undefined,
      tab,
      site: tab === 'websites' ? siteId || undefined : undefined,
      section: tab === 'research' ? researchSection : undefined,
      run: tab === 'research' && researchSection === 'provenance' ? runId || undefined : undefined,
    });
  }, [baseView, connected, modelId, researchSection, runId, selectedNodeId, siteId, tab]);

  const selectNode = useCallback(async (nodeId: string, id = modelId) => {
    if (!id) return;
    setSelectedNodeId(nodeId);
    setInspLoading(true);
    setInspError(null);
    try {
      setInspection(await api.inspectComponent(id, nodeId, { version }));
    } catch (err) {
      setInspError(err instanceof Error ? err.message : String(err));
    } finally {
      setInspLoading(false);
    }
  }, [api, modelId, version]);

  const loadModel = useCallback(async (id: string, v = 'current', name?: string, projectId?: string) => {
    if (!id) return;
    setLoadingModel(true);
    setCfError(null);
    setDiffResult(null);
    try {
      const [view, vers] = await Promise.all([api.getXRay(id, { version: v }), api.listVersions(id).catch(() => [] as GraphVersion[])]);
      setModelId(id);
      setVersion(v);
      setBaseView(view);
      setVersions(vers);
      setDrillDown(null);
      setPicking(false);
      const label = name ?? (recents.find((r) => r.modelId === id)?.name || `Model ${id.slice(0, 8)}`);
      setProjectName(label);
      setRecents(rememberModel({ modelId: id, projectId, name: label, openedAt: new Date().toISOString(), nodes: view.total_nodes }));
      const pid = projectId ?? recents.find((r) => r.modelId === id)?.projectId;
      if (pid) setMemoryProjectId(pid);
      const nodeToOpen = pendingNode.current;
      pendingNode.current = null;
      if (nodeToOpen && view.layers.some((l) => l.nodes.some((n) => n.id === nodeToOpen))) void selectNode(nodeToOpen, id);
      else { setSelectedNodeId(null); setInspection(null); }
      api.getAlternatives(id).then(setCounterfactualSet).catch(() => setCounterfactualSet(null));
    } catch (err) {
      pushToast('error', err instanceof Error ? err.message : String(err));
    } finally {
      setLoadingModel(false);
    }
  }, [api, pushToast, recents, selectNode]);

  const connect = useCallback(async () => {
    if (!tokenInfo.valid) return;
    setConnecting(true);
    setConnectError(null);
    try {
      const list = await api.listProjects();
      setProjects(list);
      setConnected(true);
      if (link.model && !baseView) await loadModel(link.model);
    } catch (err) {
      const msg = err instanceof Error ? err.message : String(err);
      setConnectError(/HTTP 401/.test(msg) ? 'The server rejected this token. It may be for another environment, or revoked.' : msg);
      setConnected(false);
    } finally {
      setConnecting(false);
    }
  }, [api, baseView, link.model, loadModel, tokenInfo.valid]);

  // Connect automatically once per complete, unexpired token (a saved one on load, or one just
  // pasted); a rejected token is not retried until it changes.
  const autoTriedFor = useRef<string | null>(null);
  useEffect(() => {
    if (connected || !tokenInfo.valid || serverStatus !== 'ready' || autoTriedFor.current === token) return;
    autoTriedFor.current = token;
    void connect();
  }, [connect, connected, serverStatus, token, tokenInfo.valid]);

  const disconnect = () => {
    setToken('');
    setConnected(false);
    setProjects(null);
    setBaseView(null);
    setInspection(null);
    setSelectedNodeId(null);
    setPicking(false);
    setSiteId('');
    setRunId('');
    writeLink({});
  };

  const openProject = useCallback(async (p: ProjectSummary) => {
    setLoadingModel(true);
    try {
      const model = await api.getModelForProject(p.id);
      await loadModel(model.id, 'current', p.name, p.id);
    } catch (err) {
      const msg = err instanceof Error ? err.message : String(err);
      pushToast('error', /HTTP 404/.test(msg) ? `${p.name} has no architecture model yet.` : msg);
      setLoadingModel(false);
    }
  }, [api, loadModel, pushToast]);

  const runDrillDown = useCallback(async (nodeId: string, depth = 2, direction: Direction = 'both') => {
    if (!modelId) return;
    try {
      setDrillDown(await api.getDrillDown(modelId, nodeId, { version, depth, direction }));
      setVisibleLayers(allLayers());
    } catch (err) {
      pushToast('error', err instanceof Error ? err.message : String(err));
    }
  }, [api, modelId, pushToast, version]);

  const generateAlternatives = useCallback(async () => {
    if (!modelId) return;
    setCfGenerating(true);
    setCfError(null);
    setDiffResult(null);
    try {
      const cf = await api.generateAlternatives(modelId, weights);
      setCounterfactualSet(cf);
      setVersions(await api.listVersions(modelId).catch(() => versions));
      pushToast('success', `Generated ${cf.alternatives.length} alternatives and recorded an ADR`);
    } catch (err) {
      const msg = err instanceof Error ? err.message : String(err);
      setCfError(msg);
      pushToast('error', msg);
    } finally {
      setCfGenerating(false);
    }
  }, [api, modelId, pushToast, versions, weights]);

  const diffAlternatives = useCallback(async (fromId: string, toId: string) => {
    if (!modelId) return;
    setDiffLoading(true);
    setCfError(null);
    try {
      setDiffResult(await api.diffAlternatives(modelId, fromId, toId));
    } catch (err) {
      setCfError(err instanceof Error ? err.message : String(err));
    } finally {
      setDiffLoading(false);
    }
  }, [api, modelId]);

  const clearSelection = () => { setSelectedNodeId(null); setInspection(null); setInspError(null); setInspTabReq(undefined); };
  const resetView = () => {
    setDrillDown(null);
    setVisibleLayers(allLayers());
    setSearch('');
    setEvidenceFilter('ALL');
  };

  // Keyboard: ⌘K palette, "/" search, Esc unwinds drill-down then selection.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const tag = (e.target as HTMLElement | null)?.tagName;
      const typing = tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT';
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k' && connected) {
        e.preventDefault();
        setPaletteOpen((v) => !v);
      } else if (e.key === 'Escape' && !paletteOpen) {
        if (drillDown) setDrillDown(null);
        else if (selectedNodeId) clearSelection();
      } else if (e.key === '/' && !typing && baseView) {
        e.preventDefault();
        document.querySelector<HTMLInputElement>('.search-input')?.focus();
      }
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [baseView, connected, drillDown, paletteOpen, selectedNodeId]);

  const activeView = drillDown ?? baseView;
  const twinNodes = useMemo(() => (baseView?.layers ?? []).flatMap((l) => l.nodes.map((n) => ({ id: n.id, name: n.name, type: n.type }))), [baseView]);
  const insights = useMemo(() => (baseView ? computeInsights(baseView) : null), [baseView]);

  const paletteItems = useMemo<PaletteItem[]>(() => {
    const items: PaletteItem[] = [];
    if (!connected) return items;
    const goto: Array<[string, string, () => void]> = [
      ['g:sites', 'Website investigations', () => setTab('websites')],
      ['g:trends', 'Research · Trends', () => { setResearchSection('trends'); setTab('research'); }],
      ['g:reports', 'Research · Reports', () => { setResearchSection('reports'); setTab('research'); }],
      ['g:knowledge', 'Research · Knowledge search', () => { setResearchSection('knowledge'); setTab('research'); }],
      ['g:memory', 'Research · Project memory', () => { setResearchSection('memory'); setTab('research'); }],
      ['g:compare', 'Research · Compare technology options', () => { setResearchSection('compare'); setTab('research'); }],
      ['g:feed', 'Research · Agent updates', () => { setResearchSection('feed'); setTab('research'); }],
      ['g:sources', 'Research · Sources', () => { setResearchSection('sources'); setTab('research'); }],
      ['g:prov', 'Research · Run provenance', () => { setResearchSection('provenance'); setTab('research'); }],
    ];
    if (baseView) goto.unshift(['g:learn', 'Learn this architecture', () => setTab('learn')], ['g:twin', 'Digital Twin · validate this architecture', () => setTab('twin')]);
    for (const [id, label, run] of goto) items.push({ id, group: 'Go to', label, run });
    if (!baseView) return items;
    for (const l of baseView.layers) {
      for (const n of l.nodes) {
        items.push({ id: `n:${n.id}`, group: 'Components', label: n.name, hint: `${n.type} · ${l.label}`, color: LAYER_BY_ID[l.id]?.color,
          keywords: n.key, run: () => { setTab('xray'); void selectNode(n.id); } });
      }
    }
    for (const l of XRAY_LAYERS) {
      items.push({ id: `l:${l.id}`, group: 'Layers', label: `Show only ${l.label}`, color: l.color, run: () => { setTab('xray'); setVisibleLayers(new Set([l.id])); } });
    }
    const actions: Array<[string, string, () => void, string?]> = [
      ['a:all', 'Show all layers', () => setVisibleLayers(allLayers())],
      ['a:alts', 'Open alternatives & decision matrix', () => setTab('counterfactual')],
      ['a:xray', 'Open X-Ray', () => setTab('xray')],
      ['a:gen', 'Generate architecture alternatives', () => { setTab('counterfactual'); void generateAlternatives(); }],
      ['a:reset', 'Reset filters and drill-down', resetView],
      ['a:switch', 'Switch architecture', () => setPicking(true)],
      ['a:theme', `Use ${document.documentElement.getAttribute('data-theme') === 'light' ? 'dark' : 'light'} theme`, () => setTheme((t) => (t === 'light' ? 'dark' : 'light'))],
    ];
    if (selectedNodeId) actions.unshift(['a:drill', 'Drill down from the selected component', () => void runDrillDown(selectedNodeId, 2, 'both')]);
    for (const [id, label, run] of actions) items.push({ id, group: 'Actions', label, run });
    return items;
  }, [baseView, connected, generateAlternatives, runDrillDown, selectNode, selectedNodeId, setTab]);

  const connectNotice = handoff === 'asking'
    ? 'Looking for a signed-in workspace tab…'
    : !connected && !tokenInfo.valid
      ? [
          initialToken.expiredAt && !token ? `Your saved sign-in expired ${new Date(initialToken.expiredAt).toLocaleString()}. Paste a fresh token to continue.` : '',
          link.model && !initialToken.expiredAt
            ? `This tab isn’t signed in, and no signed-in workspace tab is open at ${globalThis.location?.host ?? 'this address'}. Sign in once and the architecture you opened loads right away; tick “Stay signed in on this device” and links from Engineering Lens will open straight in next time.`
            : link.model ? 'The architecture you opened will load as soon as you connect.' : '',
        ].filter(Boolean).join(' ')
      : '';
  const showConnect = !connected || picking || (MODEL_TABS.includes(tab) && !baseView);

  return (
    <div className="app-shell">
      <Header
        modelLoaded={!!baseView && !picking}
        connected={connected}
        projectName={projectName}
        version={version}
        loadedVersion={baseView?.version ?? null}
        versions={versions}
        onVersionChange={(v) => void loadModel(modelId, v, projectName)}
        tab={tab}
        onTab={setTab}
        token={connected ? tokenInfo : { valid: false }}
        serverLabel={baseUrl || window.location.origin}
        theme={theme}
        onTheme={setTheme}
        onOpenPalette={() => setPaletteOpen(true)}
        onSwitchProject={() => { setPicking(true); if (connected) api.listProjects().then(setProjects).catch(() => {}); }}
        onDisconnect={disconnect}
        loading={loadingModel}
      />

      {showConnect ? (
        <ConnectScreen
          notice={connectNotice}
          baseUrl={baseUrl}
          onBaseUrlChange={setBaseUrl}
          serverStatus={serverStatus}
          token={token}
          onTokenChange={(v) => { setToken(v.trim().replace(/^Bearer\s+/i, '')); setConnectError(null); }}
          remember={remember}
          onRememberChange={setRemember}
          connected={connected}
          connecting={connecting || loadingModel}
          projects={projects}
          recents={recents}
          error={connectError}
          onConnect={() => void connect()}
          onOpenProject={(p) => void openProject(p)}
          onOpenRecent={(r) => void loadModel(r.modelId, 'current', r.name, r.projectId)}
          onOpenModelId={(id) => void loadModel(id)}
          onDisconnect={disconnect}
        />
      ) : tab === 'websites' ? (
        <WebsitesView
          api={api}
          selectedId={siteId}
          onSelect={setSiteId}
          onOpenModel={(id, name) => { void loadModel(id, 'current', name).then(() => setTab('xray')); }}
          onOpenRun={(id) => { setRunId(id); setResearchSection('provenance'); setTab('research'); }}
        />
      ) : tab === 'research' ? (
        <ResearchView api={api} section={researchSection} onSection={setResearchSection} runId={runId} onRunId={setRunId}
          projects={projects} projectId={memoryProjectId} onProjectId={setMemoryProjectId} />
      ) : tab === 'twin' ? (
        <TwinView
          api={api}
          modelId={modelId}
          version={version}
          nodes={twinNodes}
          onOpenNode={(id) => { setTab('xray'); void selectNode(id); }}
        />
      ) : tab === 'learn' ? (
        <LearnOverview
          api={api}
          modelId={modelId}
          version={version}
          onLearnComponent={(id) => {
            setTab('xray');
            setInspTabReq((r) => ({ tab: 'learn', seq: (r?.seq ?? 0) + 1 }));
            void selectNode(id);
          }}
        />
      ) : tab === 'xray' ? (
        <main className={`workspace ${sidebarOpen ? '' : 'sidebar-collapsed'} ${selectedNodeId ? 'has-selection' : ''}`}>
          <LayerSidebar
            layers={activeView?.layers ?? []}
            visibleLayers={visibleLayers}
            onToggleLayer={(id) => setVisibleLayers((prev) => { const n = new Set(prev); if (n.has(id)) n.delete(id); else n.add(id); return n; })}
            onSoloLayer={(id) => setVisibleLayers(new Set([id]))}
            onShowAllLayers={() => setVisibleLayers(allLayers())}
            search={search}
            onSearchChange={setSearch}
            evidenceFilter={evidenceFilter}
            onEvidenceFilterChange={setEvidenceFilter}
          />
          <button type="button" className="sidebar-toggle" onClick={() => setSidebarOpen((v) => !v)} aria-label={sidebarOpen ? 'Hide layers' : 'Show layers'} title={sidebarOpen ? 'Hide layers' : 'Show layers'}>
            {sidebarOpen ? '‹' : '›'}
          </button>
          {activeView && (
            <XRayCanvas
              view={activeView}
              drillDown={drillDown}
              visibleLayers={visibleLayers}
              search={search}
              evidenceFilter={evidenceFilter}
              selectedNodeId={selectedNodeId}
              onSelectNode={(id) => void selectNode(id)}
              onQuickDrillDown={(id) => void runDrillDown(id, 2, 'both')}
              onChangeDrillDown={(d, dir) => { if (drillDown) void runDrillDown(drillDown.center_node_id, d, dir); }}
              onExitDrillDown={() => setDrillDown(null)}
            />
          )}
          {selectedNodeId || inspLoading ? (
            <div className="inspector-wrap">
              <button type="button" className="inspector-close" onClick={clearSelection} aria-label="Close inspector" title="Close (Esc)">×</button>
              <InspectorDrawer
                inspection={inspection}
                loading={inspLoading}
                error={inspError}
                onSelectNode={(id) => void selectNode(id)}
                onDrillDown={(id) => void runDrillDown(id, 2, 'both')}
                onCopy={(text, label) => { void navigator.clipboard?.writeText(text); pushToast('info', `Copied ${label}`); }}
                learning={{ api, modelId, version }}
                tabRequest={inspTabReq}
              />
            </div>
          ) : insights ? (
            <InsightsPanel insights={insights} onSelectNode={(id) => void selectNode(id)} />
          ) : null}
        </main>
      ) : (
        <CounterfactualView
          modelId={modelId}
          set={counterfactualSet}
          weights={weights}
          generating={cfGenerating}
          diffResult={diffResult}
          diffLoading={diffLoading}
          error={cfError}
          onWeightChange={(c, val) => setWeights((prev) => ({ ...prev, [c]: val }))}
          onApplyPreset={(w, label) => { setWeights(w); pushToast('info', `Applied “${label}” weights`); }}
          onGenerate={() => void generateAlternatives()}
          onDiff={(a, b) => void diffAlternatives(a, b)}
        />
      )}

      <CommandPalette open={paletteOpen} items={paletteItems} onClose={() => setPaletteOpen(false)} />
      <ToastStack toasts={toasts} onDismiss={(id) => setToasts((prev) => prev.filter((t) => t.id !== id))} />
    </div>
  );
}
