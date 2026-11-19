// Connection state for the workspace: server URL, bearer token and recently opened models.
// The token lives in sessionStorage (cleared when the tab closes) unless the user explicitly
// asks to stay signed in on this device. Recent models keep only ids and names.

export interface TokenInfo {
  valid: boolean;
  problem?: string;
  subject?: string;
  tenant?: string;
  expiresAt?: number; // ms
  minutesLeft?: number;
  scope?: string;
}

function b64urlDecode(s: string): string {
  const pad = s.replace(/-/g, '+').replace(/_/g, '/');
  return atob(pad + '='.repeat((4 - (pad.length % 4)) % 4));
}

/** Reads (without verifying) a JWT's claims so the UI can explain problems before a request. */
export function inspectToken(raw: string, now = Date.now()): TokenInfo {
  const t = raw.trim().replace(/^Bearer\s+/i, '');
  if (!t) return { valid: false, problem: 'Paste a token to connect.' };
  const parts = t.split('.');
  if (parts.length !== 3) return { valid: false, problem: "That doesn't look like a token (expected three dot-separated parts)." };
  try {
    const c = JSON.parse(b64urlDecode(parts[1])) as { sub?: string; tid?: string; exp?: number; scope?: string };
    const expiresAt = c.exp ? c.exp * 1000 : undefined;
    const minutesLeft = expiresAt ? Math.round((expiresAt - now) / 60000) : undefined;
    if (expiresAt && expiresAt <= now) return { valid: false, problem: 'This token has expired. Paste a fresh one.', subject: c.sub, expiresAt };
    if (c.scope === 'lens') return { valid: false, problem: 'This is an Engineering Lens token; the workspace needs a workspace token.', scope: c.scope };
    return { valid: true, subject: c.sub, tenant: c.tid, expiresAt, minutesLeft, scope: c.scope };
  } catch {
    return { valid: false, problem: "That token couldn't be read." };
  }
}

const KEYS = {
  baseUrl: 'systera.apiBaseUrl',
  token: 'systera.apiToken',
  remember: 'systera.rememberToken',
  recents: 'systera.recentModels',
  theme: 'systera.theme',
};

function safeGet(store: Storage | undefined, k: string): string | null {
  try {
    return store?.getItem(k) ?? null;
  } catch {
    return null;
  }
}
function safeSet(store: Storage | undefined, k: string, v: string | null) {
  try {
    if (v === null) store?.removeItem(k);
    else store?.setItem(k, v);
  } catch {
    /* storage unavailable */
  }
}

export function loadBaseUrl(): string {
  return safeGet(globalThis.localStorage, KEYS.baseUrl) ?? '';
}
export function saveBaseUrl(v: string) {
  safeSet(globalThis.localStorage, KEYS.baseUrl, v);
}

/** Token from this tab's session, or from the device when the user chose to stay signed in. */
export function loadToken(): { token: string; remember: boolean } {
  const remember = safeGet(globalThis.localStorage, KEYS.remember) === '1';
  const session = safeGet(globalThis.sessionStorage, KEYS.token);
  const device = safeGet(globalThis.localStorage, KEYS.token);
  if (!remember && device) {
    // Older builds kept the token on the device by default: move it to this tab only.
    safeSet(globalThis.localStorage, KEYS.token, null);
    safeSet(globalThis.sessionStorage, KEYS.token, device);
    return { token: device, remember: false };
  }
  return { token: session ?? (remember ? device ?? '' : ''), remember };
}

export function saveToken(token: string, remember: boolean) {
  safeSet(globalThis.localStorage, KEYS.remember, remember ? '1' : null);
  safeSet(globalThis.sessionStorage, KEYS.token, token || null);
  safeSet(globalThis.localStorage, KEYS.token, remember && token ? token : null);
}

export interface RecentModel {
  modelId: string;
  projectId?: string;
  name: string;
  openedAt: string;
  nodes?: number;
}

export function loadRecents(): RecentModel[] {
  try {
    const v = JSON.parse(safeGet(globalThis.localStorage, KEYS.recents) ?? '[]');
    return Array.isArray(v) ? (v as RecentModel[]).filter((r) => r && typeof r.modelId === 'string').slice(0, 8) : [];
  } catch {
    return [];
  }
}

export function rememberModel(r: RecentModel): RecentModel[] {
  const next = [r, ...loadRecents().filter((x) => x.modelId !== r.modelId)].slice(0, 8);
  safeSet(globalThis.localStorage, KEYS.recents, JSON.stringify(next));
  return next;
}

export type Theme = 'light' | 'dark' | 'system';
export function loadTheme(): Theme {
  const v = safeGet(globalThis.localStorage, KEYS.theme);
  return v === 'light' || v === 'dark' ? v : 'system';
}
export function saveTheme(t: Theme) {
  safeSet(globalThis.localStorage, KEYS.theme, t === 'system' ? null : t);
}

/** Deep link state: ?model=&node=&tab= */
export interface LinkState {
  model?: string;
  node?: string;
  tab?: string;
  site?: string;
  section?: string;
  run?: string;
}
const LINK_KEYS = ['model', 'node', 'tab', 'site', 'section', 'run'] as const;
export function readLink(search = globalThis.location?.search ?? ''): LinkState {
  const q = new URLSearchParams(search);
  const out: LinkState = {};
  for (const k of LINK_KEYS) {
    const v = q.get(k);
    if (v) out[k] = v;
  }
  return out;
}
export function writeLink(s: LinkState) {
  try {
    const q = new URLSearchParams();
    for (const k of LINK_KEYS) {
      const v = s[k];
      if (v && !(k === 'tab' && v === 'xray')) q.set(k, v);
    }
    const qs = q.toString();
    history.replaceState(null, '', `${location.pathname}${qs ? `?${qs}` : ''}`);
  } catch {
    /* non-browser */
  }
}

// ---- Same-browser handoff between workspace tabs ----
// A new tab (for example one opened from Engineering Lens) has an empty sessionStorage. It asks
// other open workspace tabs of this origin for their token over a BroadcastChannel; a connected
// tab answers only with a valid, unexpired token. Nothing is written to disk.

const CHANNEL = 'systera.session';
type Msg = { type: 'who-has-token'; id: string } | { type: 'token'; id: string; token: string };

function channel(): BroadcastChannel | null {
  try {
    return typeof BroadcastChannel === 'undefined' ? null : new BroadcastChannel(CHANNEL);
  } catch {
    return null;
  }
}

/** Answers other tabs while this one holds a valid token. Returns a stop function. */
export function serveTokenToOtherTabs(getToken: () => string): () => void {
  const ch = channel();
  if (!ch) return () => {};
  ch.onmessage = (e: MessageEvent<Msg>) => {
    const m = e.data;
    if (m?.type !== 'who-has-token' || typeof m.id !== 'string') return;
    const token = getToken();
    if (token && inspectToken(token).valid) ch.postMessage({ type: 'token', id: m.id, token } satisfies Msg);
  };
  return () => ch.close();
}

/** Asks other open workspace tabs for a token; resolves null when none answers in time. */
export function askOtherTabsForToken(timeoutMs = 700): Promise<string | null> {
  const ch = channel();
  if (!ch) return Promise.resolve(null);
  const id = Math.random().toString(36).slice(2);
  return new Promise((resolve) => {
    const done = (t: string | null) => { globalThis.clearTimeout(timer); ch.close(); resolve(t); };
    const timer = globalThis.setTimeout(() => done(null), timeoutMs);
    ch.onmessage = (e: MessageEvent<Msg>) => {
      const m = e.data;
      if (m?.type === 'token' && m.id === id && typeof m.token === 'string' && inspectToken(m.token).valid) done(m.token);
    };
    ch.postMessage({ type: 'who-has-token', id } satisfies Msg);
  });
}
