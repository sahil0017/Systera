import React, { useEffect, useMemo, useRef, useState } from "react";
import { AnalysisMode, Consent, DeletionReport, NormalizedObservation, Session, StoredObservation, WebsiteProfile } from "../shared/types";
import { AuthSession, clearAuthSession, decodeClaims, isLensScoped, loadAuthSession, normalizeApiBase } from "../auth/session";
import { autoConfigureSession } from "../auth/autoconfig";
import { LensAPIClient } from "../api/client";
import { applyRedactions, payloadBytes, previewItems } from "../shared/redaction";
import { boundSelection, candidatePages } from "../shared/pages";
import { EXPLAIN_SCRIPTING, ensureOrigin, ensureScripting, explainApi, explainOrigin, revokeOrigin } from "../shared/permissions";
import { collectFromTab, collectPages } from "../lens/collect";
import { AlternativesView, CompareView, DiscoveryView, InvestigationView, VIEWS, ViewKey, XRayView } from "./views";

const MULTI_PAGE: AnalysisMode[] = ["product_map", "full_system_design", "compare_websites"];

function Stepper({ current }: { current: 1 | 2 | 3 }) {
  const steps = [
    { n: 1, label: "1. Connect" },
    { n: 2, label: "2. Scan & Preview" },
    { n: 3, label: "3. Findings" },
  ] as const;
  return (
    <div className="stepper" aria-label="Onboarding progress">
      {steps.map((s) => (
        <div key={s.n} className={`step-chip ${current === s.n ? "active" : current > s.n ? "done" : ""}`}>
          {current > s.n ? "✓ " : ""}{s.label}
        </div>
      ))}
    </div>
  );
}

function inspectTokenHint(raw: string): { kind: "valid" | "warn" | "idle"; text: string } {
  const trimmed = raw.trim();
  if (!trimmed) return { kind: "idle", text: "" };
  try {
    const claims = decodeClaims(trimmed);
    if (!claims) {
      return { kind: "warn", text: "Token does not look like a valid JWT (expected 3 dot-separated segments)." };
    }
    const nowSec = Math.floor(Date.now() / 1000);
    if (claims.exp && claims.exp <= nowSec) {
      return { kind: "warn", text: "This token is expired. Mint a fresh token before signing in." };
    }
    const mins = claims.exp ? Math.max(1, Math.round((claims.exp - nowSec) / 60)) : 60;
    if (isLensScoped(claims)) {
      return { kind: "valid", text: `Lens-scoped token detected (~${mins}m remaining). Ready to connect.` };
    }
    return { kind: "valid", text: `Workspace token detected (~${mins}m). Will auto-exchange once for a scoped Lens token.` };
  } catch {
    return { kind: "warn", text: "Token does not look like a valid JWT (expected 3 dot-separated segments)." };
  }
}

function SignIn({ onSignedIn }: { onSignedIn: (s: AuthSession) => void }) {
  const [api, setApi] = useState("");
  const [token, setToken] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const tokenHint = useMemo(() => inspectTokenHint(token), [token]);

  async function connectAuto() {
    setError("");
    setBusy(true);
    try {
      const session = await autoConfigureSession(api || undefined, token || undefined);
      setToken("");
      onSignedIn(session);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  return (
    <section>
      <h2>Sign in to Systera</h2>
      <p className="muted">
        Connect in one click from your active Systera Workspace. Only the short-lived lens token (at most one hour, this tenant only, browser-intelligence routes only) is kept, in memory, until the browser closes.
      </p>

      <div className="privacy-grid">
        <div className="privacy-box allow">
          <strong>Collected on Scan</strong>
          Visible headings, navigation, form labels, public routes &amp; metadata.
        </div>
        <div className="privacy-box deny">
          <strong>Never Collected</strong>
          Passwords, cookies, tokens, entered form values, or browsing history.
        </div>
      </div>

      <div className="row" style={{ marginTop: 8 }}>
        <button
          className="primary"
          style={{ width: "100%", padding: "8px 12px" }}
          onClick={connectAuto}
          disabled={busy || tokenHint.kind === "warn"}
        >
          {busy ? "Auto-configuring in background…" : "Sign in"}
        </button>
      </div>
      {error && <p className="error">{error}</p>}

      <details style={{ marginTop: 10 }}>
        <summary className="muted" style={{ fontWeight: 500 }}>Advanced: custom server or token override</summary>
        <div style={{ marginTop: 8 }}>
          <label className="field-label">Workspace API (optional override)</label>
          <input type="text" value={api} onChange={(e) => setApi(e.target.value)} placeholder="Auto-detected from active workspace" />
          <div className="preset-row">
            <button type="button" className="preset-chip" onClick={() => setApi("http://localhost:8080")}>Localhost :8080</button>
            <button type="button" className="preset-chip" onClick={() => setApi("https://")}>HTTPS Workspace</button>
          </div>
          <label className="field-label">Token (optional manual override)</label>
          <input type="password" value={token} onChange={(e) => setToken(e.target.value)} autoComplete="off" placeholder="Auto-exchanged from workspace" />
          {tokenHint.text && <div className={`token-hint ${tokenHint.kind}`}>{tokenHint.text}</div>}
          <p className="muted">{api.startsWith("http") && api.length > 8 ? safe(() => explainApi(normalizeApiBase(api))) : ""}</p>
        </div>
      </details>
    </section>
  );
}

function safe(f: () => string) {
  try {
    return f();
  } catch {
    return "";
  }
}

function Preview({ obs, removed, toggle }: { obs: NormalizedObservation; removed: Set<string>; toggle: (id: string) => void }) {
  const items = useMemo(() => previewItems(obs), [obs]);
  const sections = Array.from(new Set(items.map((i) => i.section)));
  return (
    <details open>
      <summary>{obs.url}</summary>
      <p className="muted">Uncheck anything you do not want to send. {obs.redactions_applied.length > 0 && `Already removed by Lens: ${obs.redactions_applied.join(", ")}.`}</p>
      {sections.map((s) => (
        <div key={s}>
          <strong className="muted">{s}</strong>
          {items.filter((i) => i.section === s && i.text).map((i) => (
            <label key={i.id} className={`item ${removed.has(i.id) ? "removed" : ""}`}>
              <input type="checkbox" checked={!removed.has(i.id)} onChange={() => toggle(i.id)} />
              <span>{i.text}</span>
            </label>
          ))}
        </div>
      ))}
    </details>
  );
}

export function App() {
  const [auth, setAuth] = useState<AuthSession | null>(null);
  const [consent, setConsent] = useState<Consent | null>(null);
  const [mode, setMode] = useState<AnalysisMode>("quick_scan");
  const [tabId, setTabId] = useState<number | null>(null);
  const [previews, setPreviews] = useState<NormalizedObservation[]>([]);
  const [removed, setRemoved] = useState<Record<string, Set<string>>>({});
  const [selected, setSelected] = useState<string[]>([]);
  const [session, setSession] = useState<Session | null>(null);
  const [stored, setStored] = useState<StoredObservation[]>([]);
  const [profile, setProfile] = useState<WebsiteProfile | null>(null);
  const [status, setStatus] = useState("Nothing is collected until you click Scan, and nothing is sent until you agree to the preview.");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [view, setView] = useState<ViewKey>("discovery");
  // The active tab, kept current so Scan can request access to exactly this site inside the
  // click gesture (no await before the permission prompt). The URL is only visible while Lens
  // has access to the tab (activeTab from the toolbar click, or a granted site).
  const activeTab = useRef<{ id?: number; url?: string }>({});

  const client = auth ? new LensAPIClient(auth) : null;
  const maxPages = consent?.max_pages ?? 25;

  useEffect(() => {
    loadAuthSession().then(setAuth);
  }, []);
  useEffect(() => {
    const refresh = () =>
      chrome.tabs?.query?.({ active: true, currentWindow: true })
        ?.then(([t]) => { activeTab.current = { id: t?.id, url: t?.url }; })
        .catch(() => {});
    refresh();
    const onActivated = () => refresh();
    const onUpdated = (_id: number, info: { url?: string; status?: string }) => { if (info.url || info.status === "complete") refresh(); };
    chrome.tabs?.onActivated?.addListener(onActivated);
    chrome.tabs?.onUpdated?.addListener(onUpdated);
    return () => {
      chrome.tabs?.onActivated?.removeListener(onActivated);
      chrome.tabs?.onUpdated?.removeListener(onUpdated);
    };
  }, []);
  useEffect(() => {
    if (client) client.consent().then(setConsent).catch((e) => setError(e.message));
  }, [auth]);

  async function run(label: string, f: () => Promise<void>) {
    setBusy(true);
    setError("");
    setStatus(label);
    try {
      await f();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  const scan = () =>
    run("Reading the visible structure of this page…", async () => {
      const known = activeTab.current;
      // First statement: the permission prompt stays inside the click gesture.
      if (!(await ensureScripting(known.url))) throw new Error("Scan cancelled: the collector permission was not granted.");
      const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
      if (tab?.id === undefined) throw new Error("No active tab.");
      if (tab.url && known.url && new URL(tab.url).origin !== new URL(known.url).origin) {
        await revokeOrigin(known.url);
        throw new Error("The tab changed site while asking for permission; click Scan again.");
      }
      setTabId(tab.id);
      let obs: NormalizedObservation;
      try {
        obs = await collectFromTab(tab.id);
      } catch (e) {
        if (known.url) await revokeOrigin(known.url);
        throw e;
      }
      setPreviews([obs]);
      setRemoved({});
      setSelected([]);
      setStatus("Review the preview. Nothing has been sent.");
    });

  const collectSelected = () =>
    run("Capturing the pages you selected…", async () => {
      if (tabId === null || previews.length === 0) return;
      const first = previews[0].url;
      if (!(await ensureOrigin(first))) throw new Error("Multi-page capture needs access to this site; you can still send this page alone.");
      const pages = boundSelection(selected, maxPages);
      const more = await collectPages(tabId, pages, (done, url) => setStatus(url ? `Capturing ${done + 1}/${pages.length}: ${url}` : "Done."));
      setPreviews([previews[0], ...more]);
      setStatus(`${1 + more.length} page previews ready. Nothing has been sent.`);
    });

  const cancel = async () => {
    if (previews[0]) await revokeOrigin(previews[0].url);
    setPreviews([]);
    setRemoved({});
    setSelected([]);
    setStatus("Cancelled. The previews were discarded and nothing was sent.");
  };

  const consentAndSend = () =>
    run("Sending the approved previews…", async () => {
      if (!client || !consent || previews.length === 0) return;
      const final = previews.map((p) => applyRedactions(p, removed[p.url] ?? new Set()));
      const domain = new URL(final[0].url).hostname;
      const pages = final.length > 1 ? final.map((p) => p.url) : [];
      const sess = await client.createSession(domain, mode, consent.consent_version, pages, Math.max(final.length, MULTI_PAGE.includes(mode) ? Math.min(maxPages, 10) : 1));
      setSession(sess);
      for (const o of final) await client.recordObservation(sess.id, o);
      setProfile(await client.analyze(sess.id));
      setView(mode === "compare_websites" ? "compare" : mode === "full_system_design" ? "investigation" : "discovery");
      setStored((await client.listObservations(sess.id)).items);
      await revokeOrigin(final[0].url);
      setPreviews([]);
      setStatus("Sent and analyzed. Findings are labelled observed or proposed.");
    });

  const deleteObs = (id: string) =>
    run("Deleting page…", async () => {
      if (!client || !session) return;
      const r: DeletionReport = await client.deleteObservation(id);
      setStored((await client.listObservations(session.id)).items);
      if (r.derived_profiles_deleted > 0) setProfile(null);
      setStatus(`Deleted the page and ${r.derived_profiles_deleted} finding set(s) derived from it.`);
    });

  const deleteAll = () =>
    run("Deleting investigation…", async () => {
      if (!client || !session) return;
      const r = await client.deleteSession(session.id);
      setSession(null);
      setStored([]);
      setProfile(null);
      setStatus(`Deleted: ${r.observations_deleted} page(s) and ${r.derived_profiles_deleted} derived finding set(s).`);
    });

  const reanalyze = () =>
    run("Analyzing…", async () => {
      if (client && session) setProfile(await client.analyze(session.id));
    });

  const promote = () =>
    run("Creating workspace project, 8-layer X-Ray & counterfactual matrix…", async () => {
      if (!client || !profile || !auth) return;
      const p = await client.promoteToWorkspace(profile.id);
      await client.buildFingerprint(profile.id).catch(() => {});
      await client.generateAlternatives(profile.id).catch(() => {});
      setProfile(p);
      setStatus(`Continued in workspace: project ${p.project_id}, architecture model ${p.model_id}.`);
      if (p.model_id) {
        const targetUrl = `${auth.apiBaseUrl}/?model=${encodeURIComponent(p.model_id)}`;
        try {
          await chrome.tabs?.create?.({ url: targetUrl });
        } catch {
          window.open(targetUrl, "_blank");
        }
      }
    });

  if (!auth) {
    return (
      <main>
        <div className="header-bar">
          <div className="brand-row">
            <span className="brand-dot" />
            <h1>Engineering Lens</h1>
          </div>
        </div>
        <Stepper current={1} />
        <SignIn onSignedIn={setAuth} />
      </main>
    );
  }

  const minsLeft = Math.max(1, Math.round((auth.expiresAt - Date.now()) / 60000));
  const apiHost = safe(() => new URL(auth.apiBaseUrl).host) || auth.apiBaseUrl;
  const currentStep: 1 | 2 | 3 = profile ? 3 : 2;
  const candidates = previews[0] ? candidatePages(previews[0].url, previews[0].observations.public_routes) : [];
  const bytes = previews.reduce((n, p) => n + payloadBytes(applyRedactions(p, removed[p.url] ?? new Set())), 0);

  return (
    <main>
      <div className="header-bar">
        <div className="brand-row">
          <span className="brand-dot" />
          <h1>Engineering Lens</h1>
        </div>
        <div className="row">
          <span className="session-pill" title={`Connected to ${auth.apiBaseUrl}`}>
            {apiHost} · {minsLeft}m
          </span>
          <button onClick={async () => { await clearAuthSession(); setAuth(null); }}>Sign out</button>
        </div>
      </div>

      <Stepper current={currentStep} />
      <p className="status">{status}</p>
      {error && <p className="error">{error}</p>}

      <section>
        <h2>Scan</h2>
        <label className="field-label">Mode</label>
        <select value={mode} onChange={(e) => setMode(e.target.value as AnalysisMode)} disabled={previews.length > 0}>
          <option value="quick_scan">Quick scan (this page)</option>
          <option value="product_map">Product map (pages you choose)</option>
          <option value="full_system_design">Full system design (pages you choose)</option>
          <option value="compare_websites">Compare websites (then pick other investigations)</option>
        </select>
        <p className="muted">{EXPLAIN_SCRIPTING}</p>
        <div className="row">
          <button className="primary" onClick={scan} disabled={busy}>Scan this page</button>
          {previews.length > 0 && <button onClick={cancel} disabled={busy}>Cancel</button>}
        </div>
      </section>

      {previews.length > 0 && MULTI_PAGE.includes(mode) && candidates.length > 0 && previews.length === 1 && (
        <section>
          <h2>More pages ({selected.length}/{Math.max(0, maxPages - 1)})</h2>
          <p className="muted">{explainOrigin(new URL(previews[0].url).origin)}</p>
          {candidates.slice(0, 40).map((c) => (
            <label key={c} className="item">
              <input type="checkbox" checked={selected.includes(c)}
                onChange={(e) => setSelected(e.target.checked ? boundSelection([...selected, c], maxPages) : selected.filter((x) => x !== c))} />
              <span>{new URL(c).pathname}</span>
            </label>
          ))}
          <div className="row"><button onClick={collectSelected} disabled={busy || selected.length === 0}>Capture selected pages</button></div>
        </section>
      )}

      {previews.length > 0 && (
        <section>
          <h2>Preview: what will be sent</h2>
          {previews.map((p) => (
            <Preview key={p.url} obs={p} removed={removed[p.url] ?? new Set()}
              toggle={(id) => setRemoved((r) => {
                const s = new Set(r[p.url] ?? []);
                if (s.has(id)) s.delete(id); else s.add(id);
                return { ...r, [p.url]: s };
              })} />
          ))}
          {consent && <p className="muted">{consent.text} Retention: {consent.default_retention_days} days.</p>}
          <p className="muted">{previews.length} page(s), {(bytes / 1024).toFixed(1)} KiB.</p>
          <div className="row">
            <button className="primary" onClick={consentAndSend} disabled={busy || !consent}>I agree: send {previews.length} page(s)</button>
            <button onClick={cancel} disabled={busy}>Cancel</button>
          </div>
        </section>
      )}

      {profile && (
        <section>
          <h2>Findings for {profile.domain}</h2>
          <p className="muted">{String(profile.fingerprint?.disclaimer ?? "")}</p>
          <ul>
            {profile.capabilities.map((c, i) => (
              <li key={i}><span className={`badge ${c.evidence_class}`}>{c.evidence_class}</span> {c.name} <span className="muted">({c.confidence}) {c.rationale}</span></li>
            ))}
          </ul>
          <strong className="muted">Journeys</strong>
          <ul>{profile.journeys.map((j) => <li key={j}>{j}</li>)}</ul>
          <div className="row">
            {profile.status === "PROMOTED" ? (
              <>
                <span className="muted">In workspace: project {profile.project_id}</span>
                {profile.model_id && (
                  <a
                    href={`${auth.apiBaseUrl}/?model=${encodeURIComponent(profile.model_id)}`}
                    target="_blank"
                    rel="noreferrer"
                    className="primary-link"
                    onClick={(e) => {
                      if (chrome.tabs?.create) {
                        e.preventDefault();
                        void chrome.tabs.create({ url: `${auth.apiBaseUrl}/?model=${encodeURIComponent(profile.model_id!)}` });
                      }
                    }}
                  >
                    Open in Architecture X-Ray ↗
                  </a>
                )}
              </>
            ) : (
              <button className="primary" onClick={promote} disabled={busy}>Continue in workspace</button>
            )}
          </div>
        </section>
      )}

      {profile && client && (
        <section>
          <nav className="view-tabs" role="tablist" aria-label="Investigation views">
            {VIEWS.map((v) => (
              <button key={v.key} role="tab" aria-selected={view === v.key} className={`view-tab ${view === v.key ? "active" : ""}`} onClick={() => setView(v.key)}>
                {v.label}
              </button>
            ))}
          </nav>
          {view === "discovery" && <DiscoveryView client={client} profile={profile} />}
          {view === "xray" && <XRayView client={client} profile={profile} />}
          {view === "alternatives" && <AlternativesView client={client} profile={profile} />}
          {view === "investigation" && (
            <InvestigationView client={client} profile={profile} busy={busy}
              onStarted={(p, msg) => { setProfile(p); setStatus(msg); }} />
          )}
          {view === "compare" && <CompareView client={client} profile={profile} />}
        </section>
      )}

      {session && (
        <section>
          <h2>Stored for this investigation</h2>
          <p className="muted">Deleted automatically on {new Date(session.retention_expires_at).toLocaleDateString()}.</p>
          <ul>
            {stored.map((o) => (
              <li key={o.id} className="row">
                <span style={{ flex: 1 }}>{o.url}{o.injection_suspected ? " (flagged: instruction-like text, kept as data)" : ""}</span>
                <button className="danger" onClick={() => deleteObs(o.id)} disabled={busy}>Delete</button>
              </li>
            ))}
          </ul>
          <div className="row">
            {!profile && stored.length > 0 && <button onClick={reanalyze} disabled={busy}>Analyze remaining pages</button>}
            <button className="danger" onClick={deleteAll} disabled={busy}>Delete investigation and findings</button>
          </div>
        </section>
      )}
    </main>
  );
}
