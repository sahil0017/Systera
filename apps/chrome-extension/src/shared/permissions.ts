// Minimum permissions (3.2): activeTab, storage and sidePanel are always granted; scripting
// and any host access are optional and requested only when needed, with an explanation shown
// first. Host access is removed again when an investigation ends.
import { originPattern } from "./pages";

export const EXPLAIN_SCRIPTING =
  "To read this page's visible structure, Lens needs permission to run its collector when you ask for a scan. It never runs on its own.";
export const explainOrigin = (origin: string) =>
  `To capture the pages you selected on ${origin}, Lens needs access to that site while this investigation runs. Access is removed when you finish.`;
export const explainApi = (origin: string) => `Lens sends the previews you approve to your Systera workspace at ${origin}.`;

export async function hasScripting(): Promise<boolean> {
  return chrome.permissions.contains({ permissions: ["scripting"] });
}

export async function ensureScripting(): Promise<boolean> {
  if (await hasScripting()) return true;
  return chrome.permissions.request({ permissions: ["scripting"] });
}

export async function ensureOrigin(url: string): Promise<boolean> {
  const origins = [originPattern(url)];
  if (await chrome.permissions.contains({ origins })) return true;
  return chrome.permissions.request({ origins });
}

export async function revokeOrigin(url: string): Promise<void> {
  try {
    await chrome.permissions.remove({ origins: [originPattern(url)] });
  } catch {
    /* already gone */
  }
}
