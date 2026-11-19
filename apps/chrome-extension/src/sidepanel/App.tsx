import React, { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { openInWorkspace, type TabsApi, type WindowsApi } from "../shared/workspace";
import { AnalysisMode, Consent, NormalizedObservation, Session, StoredObservation, WebsiteProfile } from "../shared/types";
import { AuthSession, clearAuthSession, loadAuthSession } from "../auth/session";
import { LensAPIClient } from "../api/client";
import { applyRedactions, payloadBytes, previewItems } from "../shared/redaction";
import { boundSelection, candidatePages } from "../shared/pages";
import { NO_TAB_ACCESS, ensureOrigin, ensureScripting, revokeOrigin } from "../shared/permissions";
import { collectFromTab, collectPages } from "../lens/collect";
import { SignIn } from "./SignIn";
import {
  ChoiceStats, MODES, MODE_INFO, ModeSuggestion, TabContext, applyLearning, classifyTab, expectedCapabilities, groupPreview,
  loadChoiceStats, pairCapabilities, personalKind, priorInvestigations, rankPages, recordChoice, suggestMode, suggestedPages,
  summarizeEvidence, timeAgo,
} from "./smart";
import { refineOnDevice } from "./ondevice";
import { loadCachedWebsites, saveCachedWebsites } from "./cache";
import { AlternativesView, Badge, CompareView, DiscoveryView, InvestigationView, VIEWS, ViewKey, XRayView } from "./views";

type Phase = "idle" | "scanning" | "review" | "capturing" | "sending" | "results";

const SCAN_STEPS = ["Asking for access to this site", "Reading visible structure", "Removing sensitive fields"];
const SEND_STEPS = ["Sending approved previews", "Classifying capabilities", "Proposing an architecture"];

function Progress({ steps, active, detail }: { steps: string[]; active: number; detail?: string }) {
  return (
    <div className="card progress-card" aria-live="polite">
      <ol className="progress-steps">
        {steps.map((s, i) => (
          <li key={s} className={i < active ? "done" : i === active ? "active" : ""}>
            <span className="dot">{i < active ? "✓" : i === active ? <span className="spinner" /> : ""}</span>
            {s}
          </li>
        ))}
      </ol>
      {detail && <p className="fine">{detail}</p>}
    </div>
  );
}

function Header({ auth, onSignOut }: { auth: AuthSession | null; onSignOut: () => void }) {
  const [menu, setMenu] = useState(false);
  const host = auth ? (() => { try { return new URL(auth.apiBaseUrl).host; } catch { return auth.apiBaseUrl; } })() : "";
  const mins = auth ? Math.max(1, Math.round((auth.expiresAt - Date.now()) / 60000)) : 0;
  return (
    <header className="topbar">
      <div className="brand"><span className="brand-dot" />Engineering Lens</div>
      {auth && (
        <div className="session">
          <button className={`session-pill ${mins <= 5 ? "warn" : ""}`} onClick={() => setMenu(!menu)} aria-expanded={menu} title={`Connected to ${auth.apiBaseUrl}`}>
            {host} · {mins}m
          </button>
          {menu && (
            <div className="menu" role="menu">
              <div className="fine">Connected to {auth.apiBaseUrl}. The Lens token expires in {mins} min.</div>
              <button role="menuitem" onClick={() => { setMenu(false); onSignOut(); }}>Sign out</button>
            </div>
          )}
        </div>
      )}
    </header>
  );
}

function SiteCard({ tab, children }: { tab: TabContext; children?: React.ReactNode }) {
  const state = classifyTab(tab);
  const host = state.kind === "ready" ? state.host : "";
  return (
    <div className="card site-card">
      <div className="site-row">
        {tab.favIconUrl && state.kind === "ready" ? <img className="favicon" src={tab.favIconUrl} alt="" /> : <span className="favicon placeholder" aria-hidden="true">◌</span>}
        <div className="site-text">
          <div className="site-host">{host || (state.kind === "restricted" ? "Can't scan this page" : "No site selected")}</div>
          <div className="fine ellipsis">{state.kind === "ready" ? tab.title || tab.url : state.kind === "restricted" ? state.reason : NO_TAB_ACCESS}</div>
        </div>
      </div>
      {children}
    </div>
  );
}

export function App() {
  const [auth, setAuth] = useState<AuthSession | null>(null);
  const [consent, setConsent] = useState<Consent | null>(null);
  const [phase, setPhase] = useState<Phase>("idle");
  const [step, setStep] = useState(0);
  const [stepDetail, setStepDetail] = useState("");
  const [mode, setMode] = useState<AnalysisMode>("quick_scan");
  const [modeTouched, setModeTouched] = useState(false);
  const [tab, setTab] = useState<TabContext>({});
  const [scanTab, setScanTab] = useState<TabContext>({});
  const [previews, setPreviews] = useState<NormalizedObservation[]>([]);
  const [removed, setRemoved] = useState<Record<string, Set<string>>>({});
  const [selected, setSelected] = useState<string[]>([]);
  const [session, setSession] = useState<Session | null>(null);
  const [stored, setStored] = useState<StoredObservation[]>([]);
  const [profile, setProfile] = useState<WebsiteProfile | null>(null);
  const [websites, setWebsites] = useState<WebsiteProfile[] | null>(null);
  const [stats, setStats] = useState<ChoiceStats>({});
  const [aiSuggestion, setAiSuggestion] = useState<(ModeSuggestion & { pages: string[] }) | null>(null);
  const [aiState, setAiState] = useState<"idle" | "thinking" | "done">("idle");
  const [view, setView] = useState<ViewKey>("discovery");
  const [notice, setNotice] = useState("");
  const [error, setError] = useState("");
  const activeTab = useRef<TabContext>({});

  const client = useMemo(() => (auth ? new LensAPIClient(auth) : null), [auth]);
  const maxPages = consent?.max_pages ?? 25;

  useEffect(() => { loadAuthSession().then(setAuth); }, []);
  useEffect(() => {
    if (!client) return;
    client.consent().then(setConsent).catch((e) => setError(e.message));
  }, [client]);

  // Keep the site card in step with the browser: tab switches and navigations.
  useEffect(() => {
    const refresh = () =>
      chrome.tabs?.query?.({ active: true, currentWindow: true })
        ?.then(([t]) => {
          const next = { id: t?.id, url: t?.url, title: t?.title, favIconUrl: t?.favIconUrl };
          activeTab.current = next;
          setTab(next);
        })
        .catch(() => {});
    refresh();
    const onActivated = () => refresh();
    const onUpdated = (_id: number, info: { url?: string; status?: string; title?: string }) => {
      if (info.url || info.title || info.status === "complete") refresh();
    };
    chrome.tabs?.onActivated?.addListener(onActivated);
    chrome.tabs?.onUpdated?.addListener(onUpdated);
    return () => {
      chrome.tabs?.onActivated?.removeListener(onActivated);
      chrome.tabs?.onUpdated?.removeListener(onUpdated);
    };
  }, []);

  const tabState = classifyTab(tab);
  const host = tabState.kind === "ready" ? tabState.host : "";

  // Previous investigations of this site (the user's own), loaded once signed in.
  // Your own investigations, fetched once per browser session (cached in memory-only session
  // storage) and refreshed after you send or delete. The request carries no site information:
  // matching it against the current tab happens here, in the panel.
  const loadWebsites = useCallback((force = false) => {
    if (!client || !auth) return;
    void (async () => {
      const cached = force ? null : await loadCachedWebsites(auth.apiBaseUrl);
      if (cached) {
        setWebsites(cached);
        return;
      }
      try {
        const r = await client.listWebsites();
        setWebsites(r.items ?? []);
        await saveCachedWebsites(auth.apiBaseUrl, r.items ?? []);
      } catch {
        setWebsites([]);
      }
    })();
  }, [client, auth]);
  useEffect(() => { loadWebsites(); }, [loadWebsites]);
  useEffect(() => { void loadChoiceStats().then(setStats); }, []);
  const prior = useMemo(() => (websites && host ? priorInvestigations(websites, host) : []), [websites, host]);
  const otherSites = useMemo(() => (websites ? new Set(websites.map((w) => w.domain)).size - (prior.length > 0 ? 1 : 0) : 0), [websites, prior]);

  const candidates = previews[0] ? candidatePages(previews[0].url, previews[0].observations.public_routes) : [];
  const ranked = useMemo(() => rankPages(candidates), [candidates.join("|")]);
  const ruleSuggestion = useMemo(() => applyLearning(suggestMode(previews[0] ?? null, candidates, otherSites), stats),
    [previews[0], candidates.join("|"), otherSites, stats]);
  const suggestion: ModeSuggestion = aiSuggestion ?? ruleSuggestion;
  const expected = useMemo(() => (previews[0] ? expectedCapabilities(previews[0]) : []), [previews[0]]);

  // Refine on-device (Chrome's built-in model) when available; never leaves the browser.
  useEffect(() => {
    if (phase !== "review" || !previews[0] || previews.length > 1) return;
    let live = true;
    setAiSuggestion(null);
    setAiState("thinking");
    void refineOnDevice(previews[0], ranked, otherSites, ruleSuggestion).then((r) => {
      if (!live) return;
      setAiState("done");
      if (r && r.source === "on-device" && ruleSuggestion.source !== "learned") setAiSuggestion(r);
    });
    return () => { live = false; };
  }, [phase === "review" && previews[0]?.url]);
  const multi = MODE_INFO[mode].multiPage;

  const fail = (e: unknown) => setError((e as Error).message);

  const reset = async () => {
    if (previews[0]) await revokeOrigin(previews[0].url);
    setPreviews([]);
    setRemoved({});
    setSelected([]);
    setModeTouched(false);
    setAiSuggestion(null);
    setAiState("idle");
    setPhase("idle");
  };

  const scan = () => {
    setError("");
    setNotice("");
    const known = activeTab.current;
    // First call in the click handler: the permission prompt keeps the user gesture.
    const asked = ensureScripting(known.url);
    setPhase("scanning");
    setStep(0);
    void (async () => {
      try {
        if (!(await asked)) throw new Error("Scan cancelled: Lens wasn't given access to this site.");
        setStep(1);
        const [t] = await chrome.tabs.query({ active: true, currentWindow: true });
        if (t?.id === undefined) throw new Error("No active tab.");
        if (t.url && known.url && new URL(t.url).origin !== new URL(known.url).origin) {
          await revokeOrigin(known.url);
          throw new Error("The tab changed site while Lens was asking for access. Scan again.");
        }
        let obs: NormalizedObservation;
        try {
          obs = await collectFromTab(t.id);
        } catch (e) {
          if (known.url) await revokeOrigin(known.url);
          throw e;
        }
        setStep(2);
        await new Promise((r) => setTimeout(r, 250));
        setScanTab({ ...known, id: t.id });
        setPreviews([obs]);
        setRemoved({});
        setSelected([]);
        setModeTouched(false);
        setPhase("review");
      } catch (e) {
        fail(e);
        setPhase("idle");
      }
    })();
  };

  // Once the preview is in, adopt Lens's suggestion until the user picks a mode themselves.
  useEffect(() => {
    if (phase !== "review" || modeTouched || previews.length > 1) return;
    setMode(suggestion.mode);
    const aiPages = aiSuggestion?.pages ?? [];
    setSelected(MODE_INFO[suggestion.mode].multiPage
      ? (aiPages.length > 0 ? boundSelection(aiPages, maxPages) : suggestedPages(candidates, Math.min(4, maxPages - 1)))
      : []);
  }, [phase, suggestion.mode, aiSuggestion, modeTouched]);

  const pickMode = (m: AnalysisMode) => {
    setModeTouched(true);
    setMode(m);
    if (!MODE_INFO[m].multiPage) setSelected([]);
    else if (selected.length === 0) setSelected(suggestedPages(candidates, Math.min(4, maxPages - 1)));
  };

  const captureSelected = () => {
    if (scanTab.id === undefined || !previews[0]) return;
    const asked = ensureOrigin(previews[0].url); // inside the click
    const pages = boundSelection(selected, maxPages);
    setPhase("capturing");
    setError("");
    void (async () => {
      try {
        if (!(await asked)) throw new Error("Capturing more pages needs access to this site; you can still send this page alone.");
        const more = await collectPages(scanTab.id!, pages, (done, url) => setStepDetail(url ? `Page ${done + 1} of ${pages.length}: ${url}` : ""));
        setPreviews([previews[0], ...more]);
        setSelected([]);
        setNotice(`${more.length} more page${more.length === 1 ? "" : "s"} captured. Review them below; nothing has been sent.`);
      } catch (e) {
        fail(e);
      } finally {
        setStepDetail("");
        setPhase("review");
      }
    })();
  };

  const send = async () => {
    if (!client || !consent || previews.length === 0) return;
    setError("");
    setPhase("sending");
    setStep(0);
    try {
      const final = previews.map((p) => applyRedactions(p, removed[p.url] ?? new Set()));
      const domain = new URL(final[0].url).hostname;
      const pages = final.length > 1 ? final.map((p) => p.url) : [];
      void recordChoice(stats, ruleSuggestion.mode, mode).then(setStats);
      const sess = await client.createSession(domain, mode, consent.consent_version, pages,
        Math.max(final.length, multi ? Math.min(maxPages, 10) : 1));
      setSession(sess);
      for (const o of final) await client.recordObservation(sess.id, o);
      setStep(1);
      const p = await client.analyze(sess.id);
      setStep(2);
      setStored((await client.listObservations(sess.id)).items);
      await revokeOrigin(final[0].url);
      setProfile(p);
      setPreviews([]);
      setView(mode === "compare_websites" ? "compare" : mode === "full_system_design" ? "investigation" : "discovery");
      setPhase("results");
      loadWebsites(true);
    } catch (e) {
      fail(e);
      setPhase("review");
    }
  };

  const openPrior = async (w: WebsiteProfile) => {
    setError("");
    setProfile(w);
    setSession(null);
    setStored([]);
    setView("discovery");
    setPhase("results");
  };

  const workspaceUrl = (p: WebsiteProfile) => (auth && p.model_id ? `${auth.apiBaseUrl}/?model=${encodeURIComponent(p.model_id)}` : "");
  const openTab = (url: string) => { void openInWorkspace(url, chrome.tabs as unknown as TabsApi | undefined, chrome.windows as unknown as WindowsApi | undefined); };

  const promote = async () => {
    if (!client || !profile) return;
    setError("");
    setNotice("Creating a workspace project with the X-Ray and alternatives…");
    try {
      const p = await client.promoteToWorkspace(profile.id);
      await client.buildFingerprint(profile.id).catch(() => {});
      await client.generateAlternatives(profile.id).catch(() => {});
      setProfile(p);
      setNotice("");
      const url = workspaceUrl(p);
      if (url) openTab(url);
    } catch (e) {
      setNotice("");
      fail(e);
    }
  };

  const deleteObs = async (id: string) => {
    if (!client || !session) return;
    try {
      const r = await client.deleteObservation(id);
      setStored((await client.listObservations(session.id)).items);
      if (r.derived_profiles_deleted > 0) setProfile(null);
      setNotice(`Deleted the page and ${r.derived_profiles_deleted} finding set(s) derived from it.`);
    } catch (e) {
      fail(e);
    }
  };

  const deleteAll = async () => {
    if (!client || !session) return;
    try {
      const r = await client.deleteSession(session.id);
      setSession(null);
      setStored([]);
      setProfile(null);
      setPhase("idle");
      setNotice(`Deleted ${r.observations_deleted} page(s) and ${r.derived_profiles_deleted} finding set(s).`);
      loadWebsites(true);
    } catch (e) {
      fail(e);
    }
  };

  const signOut = async () => {
    await clearAuthSession();
    setAuth(null);
    setPhase("idle");
    setProfile(null);
    setPreviews([]);
  };

  if (!auth) {
    return (
      <main>
        <Header auth={null} onSignOut={signOut} />
        <SignIn onSignedIn={setAuth} />
      </main>
    );
  }

  const bytes = previews.reduce((n, p) => n + payloadBytes(applyRedactions(p, removed[p.url] ?? new Set())), 0);
  const removedCount = Object.values(removed).reduce((n, s) => n + s.size, 0);
  const movedAway = (phase === "review" || phase === "results") && host && scanTab.url && classifyTab(scanTab).kind === "ready" &&
    (classifyTab(scanTab) as { host: string }).host !== host && profile?.domain.replace(/^www\./, "") !== host;

  return (
    <main className={phase === "review" || (phase === "results" && profile) ? "has-dock" : ""}>
      <Header auth={auth} onSignOut={signOut} />
      {error && <div className="banner error" role="alert"><span>{error}</span><button className="icon-btn" aria-label="Dismiss" onClick={() => setError("")}>×</button></div>}
      {notice && <div className="banner info" aria-live="polite"><span>{notice}</span><button className="icon-btn" aria-label="Dismiss" onClick={() => setNotice("")}>×</button></div>}
      {movedAway && (
        <div className="banner info">
          <span>You're now on <strong>{host}</strong>.</span>
          <button className="link-btn" onClick={() => { void reset(); setProfile(null); }}>Investigate it</button>
        </div>
      )}

      {phase === "idle" && (
        <>
          <SiteCard tab={tab}>
            <button className="primary block" onClick={scan} disabled={tabState.kind !== "ready"}>Scan {host || "this page"}</button>
            <p className="fine">Nothing is collected until you click Scan, and nothing is sent until you agree to the preview.</p>
          </SiteCard>
          {prior.length > 0 && (
            <div className="card prior">
              <div className="card-title">You've investigated {host} before</div>
              {prior.slice(0, 3).map((w) => (
                <button key={w.id} className="prior-row" onClick={() => void openPrior(w)}>
                  <span>{w.capabilities.filter((c) => c.evidence_class === "OBSERVED").length} capabilities · {w.status === "PROMOTED" ? "in workspace" : "analyzed"}</span>
                  <span className="fine">{timeAgo(w.created_at)} ›</span>
                </button>
              ))}
            </div>
          )}
          <div className="how">
            {[["1", "Scan", "Lens reads only what's visible."], ["2", "Review", "Untick anything before it's sent."], ["3", "Explore", "Capabilities, X-Ray, options."]].map(([n, t, d]) => (
              <div key={n} className="how-step"><span className="how-n">{n}</span><div><strong>{t}</strong><div className="fine">{d}</div></div></div>
            ))}
          </div>
        </>
      )}

      {phase === "scanning" && <Progress steps={SCAN_STEPS} active={step} />}
      {phase === "capturing" && <Progress steps={["Opening each selected page", "Reading visible structure", "Removing sensitive fields"]} active={1} detail={stepDetail} />}
      {phase === "sending" && <Progress steps={SEND_STEPS} active={step} />}

      {phase === "review" && previews[0] && (
        <>
          <SiteCard tab={scanTab} />
          <div className="card">
            <div className="card-title">What should Lens do?</div>
            <div className="suggest">
              <span className="spark" aria-hidden="true">✦</span>
              <div>
                <div>{suggestion.reason}</div>
                <div className="suggest-meta">
                  <span className="tag subtle">{suggestion.source === "on-device" ? "On-device AI" : suggestion.source === "learned" ? "Learned from your choices" : "Rules"}</span>
                  <span className="conf-meter" title={`${Math.round(suggestion.confidence * 100)}% confident`}><span style={{ width: `${Math.round(suggestion.confidence * 100)}%` }} /></span>
                  {aiState === "thinking" && <span className="fine"><span className="spinner tiny" /> refining on this device…</span>}
                </div>
                {suggestion.signals.length > 0 && <div className="signal-row">{suggestion.signals.map((x) => <span key={x} className="signal">{x}</span>)}</div>}
              </div>
            </div>
            {expected.length > 0 && (
              <p className="fine">Lens expects the workspace to find: <strong>{expected.map((c) => c.name).join(", ")}</strong>.</p>
            )}
            <div className="mode-grid" role="radiogroup" aria-label="Analysis mode">
              {MODES.map((m) => (
                <button key={m} role="radio" aria-checked={mode === m} className={`mode ${mode === m ? "on" : ""}`} onClick={() => pickMode(m)}>
                  <span className="mode-label">{MODE_INFO[m].label}{suggestion.mode === m && <span className="tag">Suggested</span>}</span>
                  <span className="fine">{MODE_INFO[m].short} · {MODE_INFO[m].agents} agents</span>
                </button>
              ))}
            </div>
            <p className="fine">{MODE_INFO[mode].detail}</p>
          </div>

          {multi && previews.length === 1 && ranked.length > 0 && (
            <div className="card">
              <div className="card-title row-between">
                <span>Add pages <span className="count">{selected.length}/{Math.max(0, maxPages - 1)}</span></span>
                <span className="chip-row">
                  <button className="link-btn" onClick={() => setSelected(suggestedPages(candidates, Math.min(4, maxPages - 1)))}>Suggested</button>
                  <button className="link-btn" onClick={() => setSelected([])}>None</button>
                </span>
              </div>
              <div className="page-list">
                {ranked.slice(0, 24).map((p) => (
                  <label key={p.url} className={`page-row ${selected.includes(p.url) ? "on" : ""}`}>
                    <input type="checkbox" checked={selected.includes(p.url)}
                      onChange={(e) => setSelected(e.target.checked ? boundSelection([...selected, p.url], maxPages) : selected.filter((x) => x !== p.url))} />
                    <span className="ellipsis">{p.path}</span>
                    {p.tag && p.score > 0 && <span className="tag subtle">{p.tag}</span>}
                  </label>
                ))}
              </div>
              <button className="secondary block" onClick={captureSelected} disabled={selected.length === 0}>
                Capture {selected.length} page{selected.length === 1 ? "" : "s"}
              </button>
              <p className="fine">Lens opens each page in this tab, previews it, and sends nothing until you agree.</p>
            </div>
          )}

          <div className="card">
            <div className="card-title row-between">
              <span>Preview: what will be sent</span>
              {removedCount > 0 && <span className="count">{removedCount} removed</span>}
            </div>
            {previews.map((p, pi) => (
              <PreviewPage key={p.url} obs={p} open={pi === 0} removed={removed[p.url] ?? new Set()}
                setRemoved={(s) => setRemoved((r) => ({ ...r, [p.url]: s }))} />
            ))}
          </div>

          <div className="dock">
            <div className="dock-text">
              <strong>{previews.length} page{previews.length === 1 ? "" : "s"}</strong> · {(bytes / 1024).toFixed(1)} KiB · kept {consent?.default_retention_days ?? 30}d
              {consent && <div className="fine ellipsis" title={consent.text}>{consent.text}</div>}
            </div>
            <div className="dock-actions">
              <button className="ghost" onClick={() => void reset()}>Cancel</button>
              <button className="primary" onClick={() => void send()} disabled={!consent}>I agree, send</button>
            </div>
          </div>
        </>
      )}

      {phase === "results" && profile && client && (
        <Results profile={profile} client={client} view={view} setView={setView} workspaceUrl={workspaceUrl(profile)}
          onOpen={openTab} onPromote={() => void promote()} onNew={() => { setProfile(null); void reset(); }}
          onStarted={(p, msg) => { setProfile(p); setNotice(msg); }}
          stored={stored} session={session} onDelete={(id) => void deleteObs(id)} onDeleteAll={() => void deleteAll()} />
      )}
    </main>
  );
}

function PreviewPage({ obs, open, removed, setRemoved }: {
  obs: NormalizedObservation; open: boolean; removed: Set<string>; setRemoved: (s: Set<string>) => void;
}) {
  const groups = useMemo(() => groupPreview(previewItems(obs)), [obs]);
  const toggle = (id: string) => { const s = new Set(removed); if (s.has(id)) s.delete(id); else s.add(id); setRemoved(s); };
  const setGroup = (ids: string[], keep: boolean) => { const s = new Set(removed); ids.forEach((id) => (keep ? s.delete(id) : s.add(id))); setRemoved(s); };
  let path = obs.url;
  try { path = new URL(obs.url).pathname; } catch { /* keep url */ }
  return (
    <details className="page-preview" open={open}>
      <summary><span className="ellipsis">{path}</span><span className="fine">{groups.reduce((n, g) => n + g.items.length, 0)} items</span></summary>
      {obs.redactions_applied.length > 0 && <p className="fine ok">Already removed by Lens: {obs.redactions_applied.join(", ")}.</p>}
      {groups.map((g) => {
        const kept = g.items.filter((i) => !removed.has(i.id)).length;
        const flagged = g.items.filter((i) => personalKind(i.text) && !removed.has(i.id)).length;
        return (
          <details key={g.section} className="group" open={flagged > 0 || g.items.length <= 4}>
            <summary>
              <span>{g.label}</span>
              <span className="group-meta">
                {flagged > 0 && <span className="tag warn">{flagged} check</span>}
                <span className="count">{kept}/{g.items.length}</span>
              </span>
            </summary>
            <div className="group-actions">
              <button className="link-btn" onClick={() => setGroup(g.items.map((i) => i.id), true)}>Keep all</button>
              <button className="link-btn" onClick={() => setGroup(g.items.map((i) => i.id), false)}>Remove all</button>
            </div>
            {g.items.map((i) => (
              <label key={i.id} className={`item ${removed.has(i.id) ? "removed" : ""} ${personalKind(i.text) ? "flag" : ""}`}>
                <input type="checkbox" checked={!removed.has(i.id)} onChange={() => toggle(i.id)} />
                <span>{i.text}</span>
                {personalKind(i.text) && !removed.has(i.id) && <span className="tag warn" title="Looks personal: consider removing it">{personalKind(i.text)}?</span>}
              </label>
            ))}
          </details>
        );
      })}
    </details>
  );
}

function Results({ profile, client, view, setView, workspaceUrl, onOpen, onPromote, onNew, onStarted, stored, session, onDelete, onDeleteAll }: {
  profile: WebsiteProfile; client: LensAPIClient; view: ViewKey; setView: (v: ViewKey) => void; workspaceUrl: string;
  onOpen: (url: string) => void; onPromote: () => void; onNew: () => void; onStarted: (p: WebsiteProfile, msg: string) => void;
  stored: StoredObservation[]; session: Session | null; onDelete: (id: string) => void; onDeleteAll: () => void;
}) {
  const sum = summarizeEvidence([...profile.capabilities, ...(profile.tech_signals ?? [])]);
  const pairs = pairCapabilities(profile.capabilities).filter((p) => p.capability.evidence_class !== "PROPOSED");
  return (
    <>
      <div className="card results-hero">
        <div className="row-between">
          <div>
            <div className="site-host">{profile.domain}</div>
            <div className="fine">{profile.status === "PROMOTED" ? "In your workspace" : "Analyzed"}{profile.created_at ? ` · ${timeAgo(profile.created_at)}` : ""}</div>
          </div>
          <button className="ghost small" onClick={onNew}>New scan</button>
        </div>
        <div className="evidence-bar" aria-label="Findings by evidence class">
          {(["OBSERVED", "INFERRED", "PROPOSED"] as const).map((k) => (
            <div key={k} className={`ev ${k}`}><strong>{sum[k]}</strong><span>{k.toLowerCase()}</span></div>
          ))}
        </div>
        {pairs.length > 0 && (
          <div className="cap-chips">
            {pairs.slice(0, 8).map((p) => (
              <span key={p.capability.name} className="cap-chip" title={p.component ? `Proposed: ${p.component.name}` : p.capability.rationale}>
                {p.capability.name}{p.component && <span className="arrow"> → {p.component.name}</span>}
              </span>
            ))}
          </div>
        )}
        <p className="fine">{String(profile.fingerprint?.disclaimer ?? "Hypothesis from publicly visible UI only; not a map of the real backend.")}</p>
      </div>

      <nav className="tabs" role="tablist" aria-label="Investigation views">
        {VIEWS.map((v) => (
          <button key={v.key} role="tab" aria-selected={view === v.key} className={`tab ${view === v.key ? "on" : ""}`} onClick={() => setView(v.key)}>{v.label}</button>
        ))}
      </nav>
      <div className="card">
        {view === "discovery" && <DiscoveryView client={client} profile={profile} />}
        {view === "xray" && <XRayView client={client} profile={profile} />}
        {view === "alternatives" && <AlternativesView client={client} profile={profile} />}
        {view === "investigation" && <InvestigationView client={client} profile={profile} onStarted={onStarted} />}
        {view === "compare" && <CompareView client={client} profile={profile} />}
      </div>

      {session && stored.length > 0 && (
        <details className="card disclosure">
          <summary>Your data for this investigation</summary>
          <p className="fine">Deleted automatically on {new Date(session.retention_expires_at).toLocaleDateString()}.</p>
          {stored.map((o) => (
            <div key={o.id} className="row-between stored-row">
              <span className="ellipsis">{o.url}{o.injection_suspected ? " (instruction-like text kept as data)" : ""}</span>
              <button className="danger small" onClick={() => onDelete(o.id)}>Delete</button>
            </div>
          ))}
          <button className="danger block" onClick={onDeleteAll}>Delete investigation and findings</button>
        </details>
      )}

      <div className="dock">
        {profile.status === "PROMOTED" && workspaceUrl ? (
          <button className="primary block" onClick={() => onOpen(workspaceUrl)}>Open in workspace ↗</button>
        ) : (
          <button className="primary block" onClick={onPromote}>Continue in workspace</button>
        )}
      </div>
    </>
  );
}

// Re-exported for tests that render individual pieces.
export { Badge };
