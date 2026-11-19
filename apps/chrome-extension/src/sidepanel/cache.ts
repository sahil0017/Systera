// Per-browser-session cache of the user's own investigation list. chrome.storage.session is
// memory-only and cleared when the browser closes; entries are keyed by workspace and expire
// after a few minutes, and are refreshed whenever the user sends or deletes something.
import { WebsiteProfile } from "../shared/types";

const KEY = "lens_websites_cache";
export const WEBSITES_TTL_MS = 5 * 60 * 1000;

interface Entry {
  api: string;
  at: number;
  items: WebsiteProfile[];
}

export async function loadCachedWebsites(api: string, now = Date.now()): Promise<WebsiteProfile[] | null> {
  try {
    const r = (await chrome.storage?.session?.get?.([KEY])) as unknown as Record<string, Entry | undefined> | undefined;
    const e = r?.[KEY];
    if (!e || e.api !== api || now - e.at > WEBSITES_TTL_MS || !Array.isArray(e.items)) return null;
    return e.items;
  } catch {
    return null;
  }
}

export async function saveCachedWebsites(api: string, items: WebsiteProfile[], now = Date.now()): Promise<void> {
  try {
    // Keep only what the panel needs to recognise a prior investigation.
    const slim = items.slice(0, 200).map((w) => ({ ...w, fingerprint: {}, alternatives: [] }));
    await chrome.storage?.session?.set?.({ [KEY]: { api, at: now, items: slim } satisfies Entry });
  } catch {
    /* cache is best-effort */
  }
}
