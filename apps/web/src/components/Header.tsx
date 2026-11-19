import { useEffect, useRef, useState } from 'react';
import type { Theme, TokenInfo } from '../lib/session';
import type { GraphVersion } from '../types/xray';

export type WorkspaceTab = 'xray' | 'counterfactual' | 'learn' | 'websites' | 'research';

export const MODEL_TABS: WorkspaceTab[] = ['xray', 'counterfactual', 'learn'];

const TAB_LABELS: Record<WorkspaceTab, string> = {
  xray: 'X-Ray', counterfactual: 'Alternatives', learn: 'Learn', websites: 'Websites', research: 'Research',
};

interface HeaderProps {
  modelLoaded: boolean;
  connected: boolean;
  projectName: string;
  version: string;
  loadedVersion: number | null;
  versions: GraphVersion[];
  onVersionChange: (v: string) => void;
  tab: WorkspaceTab;
  onTab: (t: WorkspaceTab) => void;
  token: TokenInfo;
  serverLabel: string;
  theme: Theme;
  onTheme: (t: Theme) => void;
  onOpenPalette: () => void;
  onSwitchProject: () => void;
  onDisconnect: () => void;
  loading: boolean;
}

const isMac = typeof navigator !== 'undefined' && /Mac|iPhone|iPad/.test(navigator.platform);

export function Header(p: HeaderProps) {
  const [menu, setMenu] = useState(false);
  const ref = useRef<HTMLDivElement | null>(null);
  useEffect(() => {
    if (!menu) return;
    const close = (e: MouseEvent) => { if (ref.current && !ref.current.contains(e.target as Node)) setMenu(false); };
    window.addEventListener('mousedown', close);
    return () => window.removeEventListener('mousedown', close);
  }, [menu]);
  const mins = p.token.minutesLeft;

  return (
    <header className="topbar">
      <div className="brand">
        <span className={`brand-dot ${p.token.valid ? 'connected' : ''}`} />
        <h1>Systera</h1>
        {p.modelLoaded && (
          <button type="button" className="crumb" onClick={p.onSwitchProject} title="Switch architecture">
            {p.projectName || 'Architecture'} <span className="caret">▾</span>
          </button>
        )}
      </div>

      {p.connected && (
        <nav className="tab-nav" aria-label="Workspace views">
          {p.modelLoaded && MODEL_TABS.map((t) => (
            <button key={t} type="button" className={`tab-btn ${p.tab === t ? 'active' : ''}`} onClick={() => p.onTab(t)}>{TAB_LABELS[t]}</button>
          ))}
          {!p.modelLoaded && (
            <button type="button" className={`tab-btn ${MODEL_TABS.includes(p.tab) ? 'active' : ''}`} onClick={() => p.onTab('xray')}>Architectures</button>
          )}
          <span className="tab-sep" aria-hidden="true" />
          {(['websites', 'research'] as WorkspaceTab[]).map((t) => (
            <button key={t} type="button" className={`tab-btn ${p.tab === t ? 'active' : ''}`} onClick={() => p.onTab(t)}>{TAB_LABELS[t]}</button>
          ))}
        </nav>
      )}

      <div className="topbar-right">
        {p.loading && <span className="spinner" aria-label="Loading" />}
        {p.modelLoaded && MODEL_TABS.includes(p.tab) && (
          <>
            <button type="button" className="palette-btn" onClick={p.onOpenPalette} title="Search components and actions">
              <span>Search or jump to…</span>
              <kbd>{isMac ? '⌘' : 'Ctrl'} K</kbd>
            </button>
            <select className="version-select" value={p.version} onChange={(e) => p.onVersionChange(e.target.value)} aria-label="Graph version">
              <option value="current">Latest{p.loadedVersion !== null && p.version === 'current' ? ` (v${p.loadedVersion})` : ''}</option>
              {p.versions.slice().sort((a, b) => b.version - a.version).map((v) => (
                <option key={v.id} value={String(v.version)}>v{v.version}{v.change_summary ? ` · ${v.change_summary.slice(0, 40)}` : ''}</option>
              ))}
            </select>
          </>
        )}
        {p.token.valid && (
          <div className="menu-wrap" ref={ref}>
            <button type="button" className={`session-pill ${mins !== undefined && mins <= 10 ? 'warn' : ''}`} onClick={() => setMenu((v) => !v)} aria-expanded={menu}>
              {p.token.subject ?? 'Connected'}{mins !== undefined ? ` · ${mins}m` : ''}
            </button>
            {menu && (
              <div className="menu" role="menu">
                <div className="menu-info">
                  <div>{p.serverLabel}</div>
                  {mins !== undefined && <div className="insp-sub">Token expires in {mins} min</div>}
                </div>
                <div className="menu-label">Theme</div>
                <div className="seg">
                  {(['system', 'light', 'dark'] as Theme[]).map((t) => (
                    <button key={t} type="button" className={p.theme === t ? 'on' : ''} onClick={() => p.onTheme(t)}>{t}</button>
                  ))}
                </div>
                {p.modelLoaded && <button type="button" role="menuitem" className="menu-item" onClick={() => { setMenu(false); p.onSwitchProject(); }}>Switch architecture</button>}
                <button type="button" role="menuitem" className="menu-item danger" onClick={() => { setMenu(false); p.onDisconnect(); }}>Disconnect</button>
              </div>
            )}
          </div>
        )}
      </div>
    </header>
  );
}
