import { test } from "node:test";
import assert from "node:assert/strict";
import { JSDOM } from "jsdom";

const b64 = (o: object) => Buffer.from(JSON.stringify(o)).toString("base64url");
const now = Math.floor(Date.now() / 1000);

async function render(sessionStore: Record<string, unknown>, tabs: Array<Record<string, unknown>> = []) {
  const dom = new JSDOM(`<div id="root"></div>`, { url: "chrome-extension://lens/sidepanel.html" });
  const g = globalThis as Record<string, unknown>;
  g.window = dom.window;
  g.document = dom.window.document;
  g.IS_REACT_ACT_ENVIRONMENT = true;
  const calls: string[] = [];
  g.chrome = {
    storage: { session: { get: async (k: string) => ({ [k]: sessionStore[k] }), set: async () => {}, remove: async () => {} } },
    permissions: { contains: async () => false, request: async () => false, remove: async () => true },
    tabs: { query: async () => tabs }, scripting: {},
  };
  g.fetch = async (url: string, init?: RequestInit) => {
    calls.push(`${init?.method ?? "GET"} ${url}`);
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

const lensSession = (mins: number) => {
  const tok = `${b64({ alg: "RS256" })}.${b64({ tid: "t", scope: "lens", iat: now, exp: now + mins * 60 })}.x`;
  return { systera_lens_session: { apiBaseUrl: "https://api.example.com", bearerToken: tok, tenantId: "t", scope: "lens", expiresAt: (now + mins * 60) * 1000 } };
};

test("signed out: one-click connect with the privacy summary, and nothing is sent", async () => {
  const signedOut = await render({});
  assert.match(signedOut.text, /Connect to Systera/);
  assert.match(signedOut.text, /Collected on Scan/);
  assert.match(signedOut.text, /Never Collected/);
  assert.match(signedOut.text, /Only a short-lived Lens token/);
  assert.match(signedOut.text, /Localhost :8080/);
  assert.equal(signedOut.calls.length, 0, "nothing is sent before sign-in");
});

test("signed in: the site card follows the active tab and only reads happen before a scan", async () => {
  const unknownTab = await render(lensSession(15));
  assert.match(unknownTab.text, /Scan this page/);
  assert.match(unknownTab.text, /Nothing is collected until you click Scan/);
  assert.match(unknownTab.text, /api\.example\.com · 15m/);
  for (const c of unknownTab.calls) assert.match(c, /^GET /, `only reads before a scan: ${c}`);
  assert.ok(unknownTab.calls.includes("GET https://api.example.com/v1/browser/consent"));

  const onSite = await render(lensSession(30), [{ id: 3, url: "https://www.shop.example.com/cart", title: "Your cart" }]);
  assert.match(onSite.text, /Scan shop\.example\.com/);
  assert.match(onSite.text, /Your cart/);

  const restricted = await render(lensSession(30), [{ id: 4, url: "chrome://extensions" }]);
  assert.match(restricted.text, /Can't scan this page/);
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
