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
