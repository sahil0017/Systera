import type { GraphVersion, ProjectSummary } from '../types/xray';

export type WorkspaceTab = 'xray' | 'counterfactual';

interface TopBarProps {
  baseUrl: string;
  token: string;
  modelId: string;
  version: string;
  loadedVersion: number | null;
  nodeCount: number;
  edgeCount: number;
  projects: ProjectSummary[];
  selectedProjectId: string;
  versions: GraphVersion[];
  activeTab: WorkspaceTab;
  loading: boolean;
  connectOpen: boolean;
  onToggleConnect: () => void;
  onBaseUrlChange: (val: string) => void;
  onTokenChange: (val: string) => void;
  onModelIdChange: (val: string) => void;
  onVersionChange: (val: string) => void;
  onSelectProject: (projectId: string) => void;
  onFetchProjects: () => void;
  onLoadModel: () => void;
  onTabChange: (tab: WorkspaceTab) => void;
  onReset: () => void;
}

export function TopBar({
  baseUrl,
  token,
  modelId,
  version,
  loadedVersion,
  nodeCount,
  edgeCount,
  projects,
  selectedProjectId,
  versions,
  activeTab,
  loading,
  connectOpen,
  onToggleConnect,
  onBaseUrlChange,
  onTokenChange,
  onModelIdChange,
  onVersionChange,
  onSelectProject,
  onFetchProjects,
  onLoadModel,
  onTabChange,
  onReset,
}: TopBarProps) {
  const connected = loadedVersion !== null;

  return (
    <>
      <header className="topbar">
        <div className="brand">
          <span className={`brand-dot ${connected ? 'connected' : ''}`} />
          <h1>
            Systera <span>Architecture Workspace</span>
          </h1>
          <nav className="tab-nav" aria-label="Workspace views">
            <button
              type="button"
              className={`tab-btn ${activeTab === 'xray' ? 'active' : ''}`}
              onClick={() => onTabChange('xray')}
            >
              Architecture X-Ray
            </button>
            <button
              type="button"
              className={`tab-btn ${activeTab === 'counterfactual' ? 'active' : ''}`}
              onClick={() => onTabChange('counterfactual')}
            >
              Counterfactual Matrix
            </button>
          </nav>
        </div>

        <div className="topbar-right">
          {connected && (
            <div className="model-pill" title={`Model ID: ${modelId}`}>
              <span className="pill-dot" />
              <span>v{loadedVersion}</span>
              <span className="pill-sep">·</span>
              <span>{nodeCount} nodes</span>
              <span className="pill-sep">·</span>
              <span>{edgeCount} edges</span>
            </div>
          )}

          {versions.length > 0 && (
            <select
              className="version-select"
              aria-label="Graph version"
              value={version}
              onChange={(e) => onVersionChange(e.target.value)}
            >
              <option value="current">Latest (current)</option>
              {versions.map((v) => (
                <option key={v.id} value={String(v.version)}>
                  v{v.version} ({v.node_count} nodes)
                </option>
              ))}
            </select>
          )}

          <button
            type="button"
            className={`btn ${connected ? 'btn-secondary' : 'btn-primary'}`}
            onClick={onToggleConnect}
          >
            {connectOpen ? 'Close Connection' : connected ? 'Connection' : 'Connect API'}
          </button>
          <button type="button" className="btn btn-ghost" onClick={onReset} title="Reset filters and drill-down (Esc)">
            Reset
          </button>
        </div>
      </header>

      {connectOpen && (
        <div className="connect-bar">
          <label>
            API Base URL
            <input
              type="text"
              value={baseUrl}
              onChange={(e) => onBaseUrlChange(e.target.value)}
              placeholder="Same origin (/v1)"
            />
          </label>
          <label>
            Bearer Token (RS256 JWT)
            <input
              type="password"
              value={token}
              onChange={(e) => onTokenChange(e.target.value)}
              placeholder="Paste Bearer token"
            />
          </label>
          <button type="button" className="btn btn-secondary" disabled={loading || !token.trim()} onClick={onFetchProjects}>
            {loading ? 'Working…' : 'Fetch Projects'}
          </button>

          {projects.length > 0 && (
            <label>
              Project
              <select value={selectedProjectId} onChange={(e) => onSelectProject(e.target.value)}>
                <option value="">Select project…</option>
                {projects.map((p) => (
                  <option key={p.id} value={p.id}>
                    {p.name}
                  </option>
                ))}
              </select>
            </label>
          )}

          <label>
            Model ID
            <input
              type="text"
              value={modelId}
              onChange={(e) => onModelIdChange(e.target.value)}
              placeholder="Architecture Model UUID"
            />
          </label>

          <button
            type="button"
            className="btn btn-primary"
            disabled={loading || !modelId.trim()}
            onClick={onLoadModel}
          >
            {loading ? 'Loading…' : 'Load Model'}
          </button>
        </div>
      )}
    </>
  );
}
