// Minimum permissions (3.2, spec §7.2): activeTab, storage and sidePanel are always granted.
// scripting and host access are optional and requested only when the user clicks Scan, with
// an explanation shown first. Host access is only ever requested for the site being
// investigated (never all sites) and is removed again when the investigation is cancelled or
// sent.
import { originPattern } from "./pages";

export const EXPLAIN_SCRIPTING =
  "To read this page's visible structure, Lens asks for permission to run its collector on this site only, when you click Scan. It never runs on its own, and access is removed when you send or cancel.";
export const explainOrigin = (origin: string) =>
  `To capture the pages you selected on ${origin}, Lens needs access to that site while this investigation runs. Access is removed when you finish.`;
export const explainApi = (origin: string) => `Lens sends the previews you approve to your Systera workspace at ${origin}.`;
export const NO_TAB_ACCESS =
  "Lens can't see this tab. Click the Engineering Lens toolbar icon while this page is open (that grants one-time access to it), then click Scan.";

/** Origins Lens may request: only http(s) sites, one at a time. */
export function scanOrigins(url: string | undefined): string[] {
  if (!url) return [];
  try {
    const u = new URL(url);
    if (u.protocol !== "https:" && u.protocol !== "http:") return [];
    return [originPattern(url)];
  } catch {
    return [];
  }
}

export async function hasScripting(): Promise<boolean> {
  return chrome.permissions.contains({ permissions: ["scripting"] });
}

/**
 * Requests the collector permission plus access to the scanned site only. Must be invoked
 * directly inside the Scan click (no preceding await) so Chrome keeps the user gesture.
 * Throws when the tab's site is unknown: Lens never falls back to asking for every site.
 */
export function ensureScripting(url: string | undefined): Promise<boolean> {
  const origins = scanOrigins(url);
  if (origins.length === 0) return Promise.reject(new Error(NO_TAB_ACCESS));
  return chrome.permissions.request({ permissions: ["scripting"], origins });
}

export function ensureOrigin(url: string): Promise<boolean> {
  return chrome.permissions.request({ origins: [originPattern(url)] });
}

export async function revokeOrigin(url: string): Promise<void> {
  try {
    await chrome.permissions.remove({ origins: [originPattern(url)] });
  } catch {
    /* already gone */
  }
}
