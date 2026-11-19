import React, { useEffect, useMemo, useState } from "react";
import { AnalysisMode, Consent, DeletionReport, NormalizedObservation, Session, StoredObservation, WebsiteProfile } from "../shared/types";
import { AuthSession, clearAuthSession, decodeClaims, isLensScoped, loadAuthSession, normalizeApiBase, saveAuthSession } from "../auth/session";
import { LensAPIClient, exchangeForLensToken } from "../api/client";
import { applyRedactions, payloadBytes, previewItems } from "../shared/redaction";
import { boundSelection, candidatePages } from "../shared/pages";
import { EXPLAIN_SCRIPTING, ensureOrigin, ensureScripting, explainApi, explainOrigin, revokeOrigin } from "../shared/permissions";
import { collectFromTab, collectPages } from "../lens/collect";

const MULTI_PAGE: AnalysisMode[] = ["product_map", "full_system_design"];

function SignIn({ onSignedIn }: { onSignedIn: (s: AuthSession) => void }) {
  const [api, setApi] = useState("https://");
  const [token, setToken] = useState("");
  const [error, setError] = useState("");
  async function submit() {
    setError("");
    try {
      const base = normalizeApiBase(api);
      if (!(await chrome.permissions.request({ origins: [`${base}/*`] }))) throw new Error("Lens needs access to your workspace API to send approved previews.");
      let lens = token.trim();
      if (!isLensScoped(decodeClaims(lens))) lens = (await exchangeForLensToken(base, lens)).access_token; // workspace token used once, not kept
      setToken("");
      onSignedIn(await saveAuthSession(base, lens));
    } catch (e) {
      setError((e as Error).message);
    }
  }
  return (
    <section>
      <h2>Sign in</h2>
      <p className="muted">Paste a lens token, or a workspace token to exchange for one. Only the short-lived lens token (at most one hour, this tenant only, browser-intelligence routes only) is kept, in memory, until the browser closes.</p>
      <label className="muted">Workspace API</label>
      <input type="text" value={api} onChange={(e) => setApi(e.target.value)} />
      <label className="muted">Token</label>
      <input type="password" value={token} onChange={(e) => setToken(e.target.value)} autoComplete="off" />
      <p className="muted">{api.startsWith("http") && api.length > 8 ? safe(() => explainApi(normalizeApiBase(api))) : ""}</p>
      <div className="row"><button className="primary" onClick={submit} disabled={!token}>Sign in</button></div>
      {error && <p className="error">{error}</p>}
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

  const client = auth ? new LensAPIClient(auth) : null;
  const maxPages = consent?.max_pages ?? 25;

  useEffect(() => {
    loadAuthSession().then(setAuth);
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
      if (!(await ensureScripting())) throw new Error("Scan cancelled: the collector permission was not granted.");
      const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
      if (tab?.id === undefined) throw new Error("No active tab.");
      setTabId(tab.id);
      const obs = await collectFromTab(tab.id);
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
    run("Creating a workspace project…", async () => {
      if (!client || !profile) return;
      const p = await client.promoteToWorkspace(profile.id);
      setProfile(p);
      setStatus(`Continued in workspace: project ${p.project_id}, architecture model ${p.model_id}.`);
    });

  if (!auth) return <main><h1>Engineering Lens</h1><SignIn onSignedIn={setAuth} /></main>;

  const candidates = previews[0] ? candidatePages(previews[0].url, previews[0].observations.public_routes) : [];
  const bytes = previews.reduce((n, p) => n + payloadBytes(applyRedactions(p, removed[p.url] ?? new Set())), 0);

  return (
    <main>
      <div className="row" style={{ justifyContent: "space-between" }}>
        <h1>Engineering Lens</h1>
        <button onClick={async () => { await clearAuthSession(); setAuth(null); }}>Sign out</button>
      </div>
      <p className="status">{status}</p>
      {error && <p className="error">{error}</p>}

      <section>
        <h2>Scan</h2>
        <label className="muted">Mode</label>
        <select value={mode} onChange={(e) => setMode(e.target.value as AnalysisMode)} disabled={previews.length > 0}>
          <option value="quick_scan">Quick scan (this page)</option>
          <option value="product_map">Product map (pages you choose)</option>
          <option value="full_system_design">Full system design (pages you choose)</option>
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
            {profile.status === "PROMOTED"
              ? <span className="muted">In workspace: project {profile.project_id}</span>
              : <button className="primary" onClick={promote} disabled={busy}>Continue in workspace</button>}
          </div>
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
