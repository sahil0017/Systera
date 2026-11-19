import { test } from "node:test";
import assert from "node:assert/strict";
import { JSDOM } from "jsdom";

const b64 = (o: object) => Buffer.from(JSON.stringify(o)).toString("base64url");
const now = Math.floor(Date.now() / 1000);

async function render(sessionStore: Record<string, unknown>) {
  const dom = new JSDOM(`<div id="root"></div>`, { url: "chrome-extension://lens/sidepanel.html" });
  const g = globalThis as Record<string, unknown>;
  g.window = dom.window;
  g.document = dom.window.document;
  g.IS_REACT_ACT_ENVIRONMENT = true;
  const calls: string[] = [];
  g.chrome = {
    storage: { session: { get: async (k: string) => ({ [k]: sessionStore[k] }), set: async () => {}, remove: async () => {} } },
    permissions: { contains: async () => false, request: async () => false, remove: async () => true },
    tabs: { query: async () => [] }, scripting: {},
  };
  g.fetch = async (url: string) => {
    calls.push(url);
    return new Response(JSON.stringify({ consent_version: "lens-consent-v1", text: "Consent text.", max_pages: 25, default_retention_days: 30, max_retention_days: 90 }));
  };
  const React = await import("react");
  const { createRoot } = await import("react-dom/client");
  const { act } = await import("react");
  const { App } = await import("../src/sidepanel/App");
  const root = createRoot(dom.window.document.getElementById("root")!);
  await act(async () => root.render(React.createElement(App)));
  await act(async () => new Promise((r) => setTimeout(r, 20)));
  const text = dom.window.document.body.textContent ?? "";
  await act(async () => root.unmount());
  dom.window.close();
  return { text, calls };
}

test("side panel asks to sign in without a lens token, and offers a scan with one", async () => {
  const signedOut = await render({});
  assert.match(signedOut.text, /Sign in/);
  assert.match(signedOut.text, /Only the short-lived lens token/);
  assert.equal(signedOut.calls.length, 0, "nothing is sent before sign-in");

  const tok = `${b64({ alg: "RS256" })}.${b64({ tid: "t", scope: "lens", iat: now, exp: now + 900 })}.x`;
  const signedIn = await render({ systera_lens_session: { apiBaseUrl: "https://api.example.com", bearerToken: tok, tenantId: "t", scope: "lens", expiresAt: (now + 900) * 1000 } });
  assert.match(signedIn.text, /Scan this page/);
  assert.match(signedIn.text, /Nothing is collected until you click Scan/);
  assert.deepEqual(signedIn.calls, ["https://api.example.com/v1/browser/consent"], "only the consent text is fetched before a scan");
});

test("onboarding stepper, privacy summary, and session expiry pill render across steps", async () => {
  const signedOut = await render({});
  assert.match(signedOut.text, /1\. Connect/);
  assert.match(signedOut.text, /2\. Scan & Preview/);
  assert.match(signedOut.text, /3\. Findings/);
  assert.match(signedOut.text, /Collected on Scan/);
  assert.match(signedOut.text, /Never Collected/);
  assert.match(signedOut.text, /Localhost :8080/);

  const tok = `${b64({ alg: "RS256" })}.${b64({ tid: "t", scope: "lens", iat: now, exp: now + 1800 })}.x`;
  const signedIn = await render({ systera_lens_session: { apiBaseUrl: "https://api.example.com", bearerToken: tok, tenantId: "t", scope: "lens", expiresAt: (now + 1800) * 1000 } });
  assert.match(signedIn.text, /✓ 1\. Connect/);
  assert.match(signedIn.text, /api\.example\.com · 30m/);
});

test("autoConfigureSession discovers workspace session in background without manual URL or token input", async () => {
  const g = globalThis as Record<string, unknown>;
  const wsTok = `${b64({ alg: "RS256" })}.${b64({ sub: "u1", tid: "t1", iat: now, exp: now + 3600 })}.sig`;
  const lensTok = `${b64({ alg: "RS256" })}.${b64({ sub: "u1", tid: "t1", scope: "lens", iat: now, exp: now + 900 })}.sig`;
  const stored: Record<string, unknown> = {};
  g.chrome = {
    storage: {
      local: { get: async () => ({ systera_workspace_pairing: { apiBaseUrl: "https://api.systera.dev", token: wsTok } }), set: async () => {}, remove: async () => {} },
      session: { set: async (v: Record<string, unknown>) => Object.assign(stored, v), get: async () => stored, remove: async () => {} },
    },
    permissions: { request: async () => true },
    tabs: { query: async () => [] },
  };
  g.fetch = async (url: string) => {
    assert.equal(url, "https://api.systera.dev/v1/auth/lens-token");
    return new Response(JSON.stringify({ access_token: lensTok, expires_at: new Date((now + 900) * 1000).toISOString() }), { status: 201 });
  };
  const { autoConfigureSession } = await import("../src/auth/autoconfig");
  const sess = await autoConfigureSession();
  assert.equal(sess.apiBaseUrl, "https://api.systera.dev");
  assert.equal(sess.scope, "lens");
  assert.equal(sess.bearerToken, lensTok);
});
