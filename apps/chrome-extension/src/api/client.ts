import {
  AnalysisMode, AnalysisStart, Consent, CounterfactualSet, DeletionReport, Finding, Fingerprint, NormalizedObservation, Session,
  StoredObservation, WebsiteComparison, WebsiteProfile,
} from "../shared/types";
import { AuthSession } from "../auth/session";

export class APIError extends Error {
  constructor(public readonly status: number, public readonly code: string, message: string) {
    super(message);
  }
}

async function call<T>(base: string, token: string, method: string, path: string, body?: unknown, extraHeaders: Record<string, string> = {}): Promise<T> {
  const res = await fetch(`${base}${path}`, {
    method,
    headers: { "Content-Type": "application/json", Authorization: `Bearer ${token}`, ...extraHeaders },
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

  private reqWithHeaders<T>(method: string, path: string, headers: Record<string, string>) {
    return call<T>(this.session.apiBaseUrl, this.session.bearerToken, method, path, undefined, headers);
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

  buildFingerprint(websiteId: string) {
    return this.req<Record<string, unknown>>("POST", `/v1/websites/${websiteId}/fingerprint`, {});
  }

  generateAlternatives(websiteId: string) {
    return this.req<Record<string, unknown>>("POST", `/v1/websites/${websiteId}/alternatives`, {});
  }

  getFingerprint(websiteId: string) {
    return this.req<Fingerprint>("GET", `/v1/websites/${websiteId}/fingerprint`);
  }

  getAlternatives(websiteId: string) {
    return this.req<CounterfactualSet>("GET", `/v1/websites/${websiteId}/alternatives`);
  }

  listFindings(websiteId: string, table: "capabilities" | "hypotheses") {
    return this.req<{ items: Finding[] }>("GET", `/v1/websites/${websiteId}/${table}`);
  }

  /** Starts the mode's multi-agent analysis run (promotes the investigation first). */
  startAnalysis(websiteId: string, idempotencyKey: string) {
    return this.reqWithHeaders<AnalysisStart>("POST", `/v1/websites/${websiteId}/analysis`, { "Idempotency-Key": idempotencyKey });
  }

  listWebsites() {
    return this.req<{ items: WebsiteProfile[] }>("GET", "/v1/websites");
  }

  /** Compares 2-5 of the user's investigations on the shared capability taxonomy (§7.6). */
  compareWebsites(websiteIds: string[]) {
    return this.req<WebsiteComparison>("POST", "/v1/websites/compare", { website_ids: websiteIds });
  }

  deleteSession(sessionId: string) {
    return this.req<DeletionReport>("DELETE", `/v1/browser/sessions/${sessionId}`);
  }
}
