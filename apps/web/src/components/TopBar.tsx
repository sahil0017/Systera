import type { GraphVersion, ProjectSummary } from '../types/xray';

export type WorkspaceTab = 'xray' | 'counterfactual';

interface TopBarProps {
  baseUrl: string;
  token: string;
  modelId: string;
  version: string;
  loadedVersion: number | null;
  projects: ProjectSummary[];
  selectedProjectId: string;
  versions: GraphVersion[];
  activeTab: WorkspaceTab;
  loading: boolean;
  statusMessage: string;
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
  projects,
  selectedProjectId,
  versions,
  activeTab,
  loading,
  statusMessage,
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
  return (
    <>
      <header className="topbar">
        <div className="brand">
          <span className="brand-dot" />
          <h1>
            Systera <span>Architecture Workspace</span>
          </h1>
          <nav className="tab-nav">
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

        <div className="topbar-meta">
          {loadedVersion !== null
            ? `Model: ${modelId} · v${loadedVersion}`
            : 'Not connected to a model'}
        </div>

        <div className="topbar-actions">
          <button type="button" className="btn btn-ghost" onClick={onReset}>
            Reset Filters
          </button>
        </div>
      </header>

      <div className="connect-bar">
        <label>
          API Base URL
          <input
            type="text"
            value={baseUrl}
            onChange={(e) => onBaseUrlChange(e.target.value)}
            placeholder="Blank uses /v1 Vite proxy"
          />
        </label>
        <label>
          Bearer Token (JWT)
          <input
            type="password"
            value={token}
            onChange={(e) => onTokenChange(e.target.value)}
            placeholder="Paste RS256 Bearer token"
          />
        </label>
        <button type="button" className="btn btn-secondary" disabled={loading} onClick={onFetchProjects}>
          Fetch Projects
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

        <label>
          Version
          {versions.length > 0 ? (
            <select value={version} onChange={(e) => onVersionChange(e.target.value)}>
              <option value="current">current</option>
              {versions.map((v) => (
                <option key={v.id} value={String(v.version)}>
                  v{v.version} ({v.node_count} nodes)
                </option>
              ))}
            </select>
          ) : (
            <input
              type="text"
              value={version}
              onChange={(e) => onVersionChange(e.target.value)}
              placeholder="current"
            />
          )}
        </label>

        <button type="button" className="btn btn-primary" disabled={loading || !modelId.trim()} onClick={onLoadModel}>
          {loading ? 'Loading…' : 'Load Model'}
        </button>
        {statusMessage && <span className="status-text">{statusMessage}</span>}
      </div>
    </>
  );
}
