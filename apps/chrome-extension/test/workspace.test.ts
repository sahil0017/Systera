import test from "node:test";
import assert from "node:assert/strict";
import { openInWorkspace, pickWorkspaceTab, saveWorkspaceOrigin, workspaceOrigin, type TabsApi } from "../src/shared/workspace";

const target = "https://systera.example.com/?model=abc";

function fakeTabs(tabs: Array<{ id?: number; windowId?: number; url?: string; active?: boolean }>, failQuery = false) {
  const calls: string[] = [];
  const api: TabsApi = {
    async query(q) { calls.push(`query ${q.url}`); if (failQuery) throw new Error("no permission"); return tabs; },
    async update(id, p) { calls.push(`update ${id} ${p.url}`); return {}; },
    async create(p) { calls.push(`create ${p.url}`); return {}; },
  };
  return { api, calls };
}

test("reuses an open workspace tab on the same server, keeping its session", async () => {
  const { api, calls } = fakeTabs([{ id: 3, windowId: 1, url: "https://systera.example.com/?tab=research" }]);
  const focused: number[] = [];
  assert.equal(await openInWorkspace(target, api, { async update(id) { focused.push(id); return {}; } }), "reused");
  assert.deepEqual(calls, ["query https://systera.example.com/*", `update 3 ${target}`]);
  assert.deepEqual(focused, [1]);
});

test("ignores API pages and other servers, then opens a new tab", async () => {
  const { api, calls } = fakeTabs([
    { id: 1, url: "https://systera.example.com/v1/projects" },
    { id: 2, url: "https://other.example.com/" },
    { id: 4 },
  ]);
  assert.equal(await openInWorkspace(target, api), "created");
  assert.equal(calls.at(-1), `create ${target}`);
});

test("falls back to a new tab when tabs cannot be queried", async () => {
  const { api, calls } = fakeTabs([], true);
  assert.equal(await openInWorkspace(target, api), "created");
  assert.equal(calls.at(-1), `create ${target}`);
});

test("prefers the active workspace tab when several are open", () => {
  const t = pickWorkspaceTab([
    { id: 5, url: "https://systera.example.com/" },
    { id: 6, url: "https://systera.example.com/index.html?model=x", active: true },
  ], target);
  assert.equal(t?.id, 6);
});

function memStore() {
  const data: Record<string, unknown> = {};
  return { data, async get(k: string) { return { [k]: data[k] }; }, async set(v: Record<string, unknown>) { Object.assign(data, v); } };
}

test("opens the workspace where Lens was paired, not the API origin", async () => {
  const store = memStore();
  assert.equal(await workspaceOrigin("http://localhost:8080", store), "http://localhost:8080"); // nothing recorded yet
  await saveWorkspaceOrigin("http://localhost:8080", "http://localhost:5173/?model=x", store);
  assert.equal(await workspaceOrigin("http://localhost:8080", store), "http://localhost:5173");
  // A different API (another server) does not reuse the recorded workspace.
  assert.equal(await workspaceOrigin("https://systera.example.com", store), "https://systera.example.com");
});

test("only http(s) origins are recorded", async () => {
  const store = memStore();
  await saveWorkspaceOrigin("http://localhost:8080", "chrome-extension://abc/sidepanel.html", store);
  await saveWorkspaceOrigin("http://localhost:8080", "not a url", store);
  assert.deepEqual(store.data, {});
  assert.equal(await workspaceOrigin("http://localhost:8080", store), "http://localhost:8080");
});
