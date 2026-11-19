import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync, readdirSync } from "node:fs";
import { join } from "node:path";

test("manifest asks only for the minimum permissions (3.2)", () => {
  const m = JSON.parse(readFileSync("manifest.json", "utf8"));
  assert.equal(m.manifest_version, 3);
  assert.deepEqual([...m.permissions].sort(), ["activeTab", "sidePanel", "storage"]);
  assert.deepEqual(m.optional_permissions, ["scripting"]);
  assert.equal(m.host_permissions, undefined, "no host access up front");
  assert.equal(m.content_scripts, undefined, "no always-on content scripts");
  for (const forbidden of ["tabs", "cookies", "history", "webRequest", "webRequestBlocking", "debugger", "<all_urls>", "downloads", "clipboardRead"]) {
    assert.ok(!JSON.stringify(m.permissions).includes(forbidden), forbidden);
  }
  assert.match(m.content_security_policy.extension_pages, /script-src 'self'/);
});

function sources(dir: string): string[] {
  return readdirSync(dir, { withFileTypes: true }).flatMap((e) => (e.isDirectory() ? sources(join(dir, e.name)) : e.name.endsWith(".ts") ? [join(dir, e.name)] : []));
}

test("collector code never touches values, cookies, storage, history or the network (3.3)", () => {
  const banned: [RegExp, string][] = [
    [/\.value\b/, "form values"], [/document\.cookie/, "cookies"], [/localStorage|sessionStorage|indexedDB|caches\./, "storage"],
    [/\bhistory\./, "history"], [/\bfetch\(|XMLHttpRequest|sendBeacon|WebSocket/, "network"], [/innerHTML|outerHTML|innerText/, "raw markup / hidden text"],
    [/chrome\.(cookies|history|webRequest)/, "privileged APIs"],
  ];
  for (const f of [...sources("src/content"), ...sources("src/collector"), ...sources("src/browser-adapter")]) {
    const code = readFileSync(f, "utf8").replace(/\/\/.*$/gm, "").replace(/\/\*[\s\S]*?\*\//g, "");
    for (const [re, what] of banned) assert.ok(!re.test(code), `${f} uses ${what} (${re})`);
  }
});

test("Scan requests the collector for the scanned site only, never all sites (§7.2)", async () => {
  const g = globalThis as Record<string, unknown>;
  const requests: chrome.permissions.Permissions[] = [];
  g.chrome = { permissions: { request: async (p: chrome.permissions.Permissions) => { requests.push(p); return true; } } };
  const { ensureScripting, scanOrigins } = await import("../src/shared/permissions");
  assert.equal(await ensureScripting("https://shop.example.com/cart?x=1"), true);
  assert.deepEqual(requests, [{ permissions: ["scripting"], origins: ["https://shop.example.com/*"] }]);
  for (const bad of [undefined, "chrome://extensions", "file:///etc/passwd", "not a url"]) {
    await assert.rejects(ensureScripting(bad), /can't see this tab/);
    assert.deepEqual(scanOrigins(bad), []);
  }
  assert.equal(requests.length, 1, "no fallback request for unknown tabs");
  for (const r of requests) assert.ok(!(r.origins ?? []).some((o) => o.includes("*://") || o === "https://*/*" || o === "http://*/*"));
});
