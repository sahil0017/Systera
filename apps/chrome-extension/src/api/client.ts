import { AnalysisMode, Consent, DeletionReport, NormalizedObservation, Session, StoredObservation, WebsiteProfile } from "../shared/types";
import { AuthSession } from "../auth/session";

export class APIError extends Error {
  constructor(public readonly status: number, public readonly code: string, message: string) {
    super(message);
  }
}

async function call<T>(base: string, token: string, method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(`${base}${path}`, {
    method,
    headers: { "Content-Type": "application/json", Authorization: `Bearer ${token}` },
    body: body === undefined ? undefined : JSON.stringify(body),
    credentials: "omit",
    cache: "no-store",
  });
  const text = await res.text();
  let data: Record<string, string> = {};
  try {
    data = text ? JSON.parse(text) : {};
  } catch {
    data = { error: text.slice(0, 200) };
  }
  if (!res.ok) throw new APIError(res.status, data.code ?? "error", data.error ?? data.message ?? `HTTP ${res.status}`);
  return data as T;
}

/** Exchanges a workspace token (used once, never stored) for a lens-scoped token. */
export function exchangeForLensToken(apiBase: string, workspaceToken: string) {
  return call<{ access_token: string; expires_at: string }>(apiBase, workspaceToken.trim(), "POST", "/v1/auth/lens-token", {});
}

export class LensAPIClient {
  constructor(private readonly session: AuthSession) {}

  private req<T>(method: string, path: string, body?: unknown) {
    return call<T>(this.session.apiBaseUrl, this.session.bearerToken, method, path, body);
  }

  consent() {
    return this.req<Consent>("GET", "/v1/browser/consent");
  }

  createSession(domain: string, mode: AnalysisMode, consentVersion: string, allowedPages: string[], maxPages: number) {
    return this.req<Session>("POST", "/v1/browser/sessions", {
      domain, analysis_mode: mode, allowed_pages: allowedPages, max_pages: maxPages,
      consent_confirmed: true, consent_version: consentVersion,
    });
  }

  recordObservation(sessionId: string, obs: NormalizedObservation) {
    return this.req<StoredObservation>("POST", "/v1/browser/observations", { ...obs, session_id: sessionId });
  }

  listObservations(sessionId: string) {
    return this.req<{ items: StoredObservation[] }>("GET", `/v1/browser/sessions/${sessionId}/observations`);
  }

  deleteObservation(id: string) {
    return this.req<DeletionReport>("DELETE", `/v1/browser/observations/${id}`);
  }

  analyze(sessionId: string) {
    return this.req<WebsiteProfile>("POST", "/v1/browser/analyze", { session_id: sessionId });
  }

  promoteToWorkspace(websiteId: string) {
    return this.req<WebsiteProfile>("POST", `/v1/websites/${websiteId}/promote`, {});
  }

  deleteSession(sessionId: string) {
    return this.req<DeletionReport>("DELETE", `/v1/browser/sessions/${sessionId}`);
  }
}
