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
