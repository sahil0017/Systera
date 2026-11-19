// Extension auth (3.5): only short-lived, tenant-pinned "lens"-scoped RS256 tokens are kept,
// in chrome.storage.session (memory only, cleared when the browser closes). A workspace token
// may be pasted once to exchange for a lens token; it is never stored.
export interface AuthSession {
  apiBaseUrl: string;
  bearerToken: string;
  tenantId: string;
  scope: string;
  expiresAt: number; // ms
}

export interface TokenClaims {
  sub?: string;
  tid?: string;
  scope?: string;
  exp?: number;
  iat?: number;
}

export const MAX_LENS_TOKEN_SECONDS = 3600;
const SESSION_KEY = "systera_lens_session";

export function decodeClaims(token: string): TokenClaims | null {
  const parts = token.trim().split(".");
  if (parts.length !== 3) return null;
  try {
    const b64 = parts[1].replace(/-/g, "+").replace(/_/g, "/");
    const json = atob(b64 + "=".repeat((4 - (b64.length % 4)) % 4));
    return JSON.parse(json) as TokenClaims;
  } catch {
    return null;
  }
}

export function isLensScoped(c: TokenClaims | null): boolean {
  return !!c && (c.scope ?? "").split(/\s+/).includes("lens");
}

/** Validates a token for storage; returns why it is unacceptable, or null if fine. */
export function lensTokenProblem(token: string, nowMs = Date.now()): string | null {
  const c = decodeClaims(token);
  if (!c) return "That is not a JWT.";
  if (!isLensScoped(c)) return "Only lens-scoped tokens are stored. Paste a workspace token to exchange it instead.";
  if (!c.tid) return "Lens tokens must be pinned to a tenant.";
  if (!c.exp || !c.iat || c.exp - c.iat > MAX_LENS_TOKEN_SECONDS) return "Lens tokens must live at most one hour.";
  if (c.exp * 1000 <= nowMs) return "This token has expired.";
  return null;
}

export function normalizeApiBase(raw: string): string {
  const u = new URL(raw.trim());
  if (u.protocol !== "https:" && !(u.protocol === "http:" && (u.hostname === "localhost" || u.hostname === "127.0.0.1"))) {
    throw new Error("The API must use https (http is allowed only for localhost).");
  }
  return u.origin;
}

export async function saveAuthSession(apiBaseUrl: string, token: string): Promise<AuthSession> {
  const problem = lensTokenProblem(token);
  if (problem) throw new Error(problem);
  const c = decodeClaims(token)!;
  const sess: AuthSession = { apiBaseUrl, bearerToken: token.trim(), tenantId: c.tid!, scope: c.scope!, expiresAt: c.exp! * 1000 };
  await chrome.storage.session.set({ [SESSION_KEY]: sess });
  return sess;
}

export async function loadAuthSession(): Promise<AuthSession | null> {
  const data = await chrome.storage.session.get(SESSION_KEY);
  const sess = data[SESSION_KEY] as AuthSession | undefined;
  if (!sess || Date.now() >= sess.expiresAt - 5000) {
    if (sess) await clearAuthSession();
    return null;
  }
  return sess;
}

export async function clearAuthSession(): Promise<void> {
  await chrome.storage.session.remove(SESSION_KEY);
}
