// Opening the Systera web workspace from the side panel.
//
// The workspace keeps its sign-in per tab (sessionStorage), so a brand-new tab starts signed
// out. When a workspace tab is already open on the same server we navigate that tab instead,
// which keeps its session; otherwise we open a new one (and the new tab asks other open
// workspace tabs for a session itself).

type Tab = { id?: number; windowId?: number; url?: string; active?: boolean };
export interface TabsApi {
  query(q: { url: string }): Promise<Tab[]>;
  update(id: number, p: { url: string; active: boolean }): Promise<unknown>;
  create(p: { url: string }): Promise<unknown>;
}
export interface WindowsApi {
  update(id: number, p: { focused: boolean }): Promise<unknown>;
}

// Where the user's workspace actually runs. It is often not the API origin (the Vite dev server
// on :5173 proxying to :8080, or localhost vs 127.0.0.1), and a tab on another origin shares no
// session with the signed-in one. Recorded when Lens pairs from a workspace tab; not a secret.
const WORKSPACE_KEY = "systera_workspace_origin";
interface WorkspaceRecord { apiBaseUrl: string; origin: string }
type StorageLike = { get(k: string): Promise<Record<string, unknown>>; set(v: Record<string, unknown>): Promise<void> };

export function httpOrigin(raw: string | undefined): string | null {
  if (!raw) return null;
  try {
    const u = new URL(raw);
    return u.protocol === "https:" || u.protocol === "http:" ? u.origin : null;
  } catch {
    return null;
  }
}

export async function saveWorkspaceOrigin(apiBaseUrl: string, origin: string | null, store?: StorageLike): Promise<void> {
  const o = httpOrigin(origin ?? undefined);
  if (!o || !store) return;
  await store.set({ [WORKSPACE_KEY]: { apiBaseUrl, origin: o } satisfies WorkspaceRecord }).catch(() => {});
}

/** The origin to open the workspace at for this API: the recorded one for the same API, else the API itself. */
export async function workspaceOrigin(apiBaseUrl: string, store?: StorageLike): Promise<string> {
  try {
    const rec = (await store?.get(WORKSPACE_KEY))?.[WORKSPACE_KEY] as WorkspaceRecord | undefined;
    if (rec && rec.apiBaseUrl === apiBaseUrl) return httpOrigin(rec.origin) ?? apiBaseUrl;
  } catch {
    // fall through
  }
  return apiBaseUrl;
}

/** A tab showing the workspace app itself (not an API response) on the target's origin. */
export function pickWorkspaceTab(tabs: Tab[], targetUrl: string): Tab | undefined {
  const origin = new URL(targetUrl).origin;
  const candidates = tabs.filter((t) => {
    if (t.id === undefined || !t.url) return false;
    try {
      const u = new URL(t.url);
      return u.origin === origin && (u.pathname === "/" || u.pathname === "/index.html");
    } catch {
      return false;
    }
  });
  return candidates.find((t) => t.active) ?? candidates[0];
}

export async function openInWorkspace(url: string, tabs?: TabsApi, windows?: WindowsApi): Promise<"reused" | "created"> {
  if (!tabs) {
    window.open(url, "_blank");
    return "created";
  }
  try {
    const found = pickWorkspaceTab(await tabs.query({ url: `${new URL(url).origin}/*` }), url);
    if (found?.id !== undefined) {
      await tabs.update(found.id, { url, active: true });
      if (found.windowId !== undefined) await windows?.update(found.windowId, { focused: true }).catch(() => {});
      return "reused";
    }
  } catch {
    // No permission to see that origin's tabs, or the tab went away: open a new one.
  }
  await tabs.create({ url });
  return "created";
}
