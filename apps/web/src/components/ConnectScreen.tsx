import { useMemo, useState } from 'react';
import { inspectToken, type RecentModel } from '../lib/session';
import type { ProjectSummary } from '../types/xray';

export type ServerStatus = 'checking' | 'ready' | 'not_ready' | 'unreachable';

interface ConnectScreenProps {
  /** Context shown above the form, e.g. an expired sign-in or a pending deep link. */
  notice?: string;
  baseUrl: string;
  onBaseUrlChange: (v: string) => void;
  serverStatus: ServerStatus;
  token: string;
  onTokenChange: (v: string) => void;
  remember: boolean;
  onRememberChange: (v: boolean) => void;
  connected: boolean;
  connecting: boolean;
  projects: ProjectSummary[] | null;
  recents: RecentModel[];
  error: string | null;
  onConnect: () => void;
  onOpenProject: (p: ProjectSummary) => void;
  onOpenRecent: (r: RecentModel) => void;
  onOpenModelId: (id: string) => void;
  onDisconnect: () => void;
}

const STATUS_TEXT: Record<ServerStatus, string> = {
  checking: 'Checking server…',
  ready: 'Server ready',
  not_ready: 'Server reachable but not ready',
  unreachable: 'Server unreachable',
};

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

function ago(iso: string) {
  const s = Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000);
  if (s < 3600) return `${Math.max(1, Math.round(s / 60))} min ago`;
  if (s < 86400) return `${Math.round(s / 3600)} h ago`;
  return `${Math.round(s / 86400)} d ago`;
}

export function ConnectScreen(p: ConnectScreenProps) {
  const info = useMemo(() => inspectToken(p.token), [p.token]);
  const [filter, setFilter] = useState('');
  const [modelId, setModelId] = useState('');
  const [showServer, setShowServer] = useState(p.serverStatus === 'unreachable');
  const projects = (p.projects ?? []).filter((x) => !filter || x.name.toLowerCase().includes(filter.toLowerCase()));

  return (
    <div className="connect">
      <div className="connect-inner">
        <div className="connect-hero">
          <div className="hero-mark" aria-hidden="true">◈</div>
          <h1>Architecture workspace</h1>
          <p>Explore any architecture model layer by layer, inspect its components, compare alternatives and learn why each part exists.</p>
        </div>

        {!p.connected ? (
          <div className="surface connect-card">
            <div className="step-head">
              <span className="step-n">1</span>
              <div>
                <strong>Connect to your control plane</strong>
                <div className={`status-line ${p.serverStatus}`}>
                  <span className="status-dot" /> {STATUS_TEXT[p.serverStatus]} · {p.baseUrl || 'this server'}
                  <button type="button" className="link-btn" onClick={() => setShowServer((v) => !v)}>{showServer ? 'Hide' : 'Change'}</button>
                </div>
              </div>
            </div>
            {p.notice && <div className="connect-notice">{p.notice}</div>}
            {showServer && (
              <label className="field">
                <span>API base URL</span>
                <input value={p.baseUrl} onChange={(e) => p.onBaseUrlChange(e.target.value)} placeholder="Leave empty to use this server" />
              </label>
            )}
            <label className="field">
              <span>Access token</span>
              <input type="password" autoComplete="off" value={p.token} onChange={(e) => p.onTokenChange(e.target.value)}
                placeholder="Paste a workspace token (JWT)" onKeyDown={(e) => { if (e.key === 'Enter' && info.valid) p.onConnect(); }} />
            </label>
            {p.token && (
              <div className={`token-note ${info.valid ? 'ok' : 'warn'}`}>
                {info.valid
                  ? `Token for ${info.subject ?? 'unknown user'}${info.minutesLeft !== undefined ? ` · expires in ${info.minutesLeft} min` : ''}`
                  : info.problem}
              </div>
            )}
            <details className="hint">
              <summary>Where do I get a token?</summary>
              <p>Ask your workspace admin, or for local development run <code>make token EMAIL=you@acme.test</code> in the repository.</p>
            </details>
            <div className="connect-actions">
              <label className="check">
                <input type="checkbox" checked={p.remember} onChange={(e) => p.onRememberChange(e.target.checked)} /> Stay signed in on this device
              </label>
              <button type="button" className="btn btn-primary" disabled={!info.valid || p.connecting || p.serverStatus === 'unreachable'} onClick={p.onConnect}>
                {p.connecting ? 'Connecting…' : 'Connect'}
              </button>
            </div>
            {p.error && <div className="cf-error">{p.error}</div>}
          </div>
        ) : (
          <div className="surface connect-card">
            <div className="step-head">
              <span className="step-n done">✓</span>
              <div className="grow">
                <strong>Choose an architecture</strong>
                <div className="status-line ready">
                  <span className="status-dot" /> Connected as {info.subject ?? 'you'}{info.minutesLeft !== undefined ? ` · ${info.minutesLeft} min left` : ''}
                  <button type="button" className="link-btn" onClick={p.onDisconnect}>Disconnect</button>
                </div>
              </div>
            </div>
            {p.recents.length > 0 && (
              <div className="recents">
                <div className="section-label">Recent</div>
                {p.recents.slice(0, 4).map((r) => (
                  <button key={r.modelId} type="button" className="row-btn" onClick={() => p.onOpenRecent(r)}>
                    <span className="row-title">{r.name}</span>
                    <span className="row-meta">{r.nodes !== undefined ? `${r.nodes} components · ` : ''}{ago(r.openedAt)}</span>
                  </button>
                ))}
              </div>
            )}
            <div className="section-label row-between">
              <span>Projects{p.projects ? ` (${p.projects.length})` : ''}</span>
              {(p.projects?.length ?? 0) > 6 && <input className="mini-search" placeholder="Filter…" value={filter} onChange={(e) => setFilter(e.target.value)} />}
            </div>
            {p.projects === null ? (
              <div className="skeleton-list"><div className="skeleton-line wide" /><div className="skeleton-line" /><div className="skeleton-line short" /></div>
            ) : projects.length === 0 ? (
              <div className="empty-note">{filter ? 'No projects match.' : 'No projects yet. Create one through the API, or send an investigation from Engineering Lens.'}</div>
            ) : (
              <div className="project-grid">
                {projects.map((x) => (
                  <button key={x.id} type="button" className="project-card" onClick={() => p.onOpenProject(x)}>
                    <span className="row-title">{x.name}</span>
                    {x.description && <span className="row-meta clamp">{x.description}</span>}
                  </button>
                ))}
              </div>
            )}
            <details className="hint">
              <summary>Open a model by id</summary>
              <div className="inline-form">
                <input value={modelId} onChange={(e) => setModelId(e.target.value.trim())} placeholder="Architecture model UUID" />
                <button type="button" className="btn btn-secondary" disabled={!UUID.test(modelId)} onClick={() => p.onOpenModelId(modelId)}>Open</button>
              </div>
            </details>
            {p.error && <div className="cf-error">{p.error}</div>}
          </div>
        )}
      </div>
    </div>
  );
}
