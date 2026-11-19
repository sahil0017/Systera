import test from "node:test";
import assert from "node:assert/strict";
import { openInWorkspace, pickWorkspaceTab, type TabsApi } from "../src/shared/workspace";

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
