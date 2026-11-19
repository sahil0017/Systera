import React, { useMemo, useState } from "react";
import { AuthSession, decodeClaims, isLensScoped, normalizeApiBase } from "../auth/session";
import { autoConfigureSession } from "../auth/autoconfig";
import { explainApi } from "../shared/permissions";

export function inspectTokenHint(raw: string): { kind: "valid" | "warn" | "idle"; text: string } {
  const trimmed = raw.trim();
  if (!trimmed) return { kind: "idle", text: "" };
  const claims = decodeClaims(trimmed);
  if (!claims) return { kind: "warn", text: "This doesn't look like a token (expected three dot-separated parts)." };
  const nowSec = Math.floor(Date.now() / 1000);
  if (claims.exp && claims.exp <= nowSec) return { kind: "warn", text: "This token has expired. Get a fresh one from your workspace." };
  const mins = claims.exp ? Math.max(1, Math.round((claims.exp - nowSec) / 60)) : 60;
  return isLensScoped(claims)
    ? { kind: "valid", text: `Lens token, about ${mins} min left.` }
    : { kind: "valid", text: `Workspace token (${mins} min). Lens exchanges it once for a short-lived Lens token and doesn't keep it.` };
}

function safe(f: () => string) {
  try {
    return f();
  } catch {
    return "";
  }
}

export function SignIn({ onSignedIn }: { onSignedIn: (s: AuthSession) => void }) {
  const [api, setApi] = useState("");
  const [token, setToken] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [advanced, setAdvanced] = useState(false);
  const tokenHint = useMemo(() => inspectTokenHint(token), [token]);

  async function connect() {
    setError("");
    setBusy(true);
    try {
      const session = await autoConfigureSession(api || undefined, token || undefined);
      setToken("");
      onSignedIn(session);
    } catch (e) {
      setError((e as Error).message);
      setAdvanced(true); // the one-click path failed: show the manual options right away
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="stack">
      <div className="hero-card">
        <div className="hero-icon" aria-hidden="true">◎</div>
        <h2>See how any website could be built</h2>
        <p className="muted">Lens reads what's visible on a page you choose, then your Systera workspace proposes an architecture for it, labelled observed, inferred or proposed.</p>
        <button className="primary block" onClick={connect} disabled={busy || tokenHint.kind === "warn"}>
          {busy ? <><span className="spinner" /> Connecting to your workspace…</> : "Connect to Systera"}
        </button>
        {error && <p className="error" role="alert">{error}</p>}
      </div>

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
      <p className="fine">Only a short-lived Lens token (an hour at most, this workspace only) is kept, in memory, until the browser closes.</p>

      <details className="disclosure" open={advanced} onToggle={(e) => setAdvanced((e.target as HTMLDetailsElement).open)}>
        <summary>Use a specific server or token</summary>
        <label className="field-label" htmlFor="api">Workspace API</label>
        <input id="api" type="text" value={api} onChange={(e) => setApi(e.target.value)} placeholder="Detected from your open workspace" />
        <div className="chip-row">
          <button type="button" className="chip" onClick={() => setApi("http://localhost:8080")}>Localhost :8080</button>
          <button type="button" className="chip" onClick={() => setApi("https://")}>HTTPS Workspace</button>
        </div>
        <label className="field-label" htmlFor="tok">Token</label>
        <input id="tok" type="password" value={token} onChange={(e) => setToken(e.target.value)} autoComplete="off" placeholder="Paste a workspace or Lens token" />
        {tokenHint.text && <div className={`token-hint ${tokenHint.kind}`}>{tokenHint.text}</div>}
        {api.startsWith("http") && api.length > 8 && <p className="fine">{safe(() => explainApi(normalizeApiBase(api)))}</p>}
      </details>
    </div>
  );
}
