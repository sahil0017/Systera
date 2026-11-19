/**
 * Multi-page investigation (3.9): candidates come from the scanned page's own public routes,
 * stay on its origin, and the selection is bounded by the session's page limit.
 */
export function candidatePages(pageUrl: string, routes: string[]): string[] {
  let origin: string;
  let current: string;
  try {
    const u = new URL(pageUrl);
    origin = u.origin;
    current = normalizePage(u.toString());
  } catch {
    return [];
  }
  const out: string[] = [];
  for (const r of routes) {
    if (!r.startsWith("/") || r.startsWith("//")) continue;
    const p = normalizePage(origin + r);
    if (p && p !== current && !out.includes(p)) out.push(p);
  }
  return out;
}

/** scheme://host/path without query, fragment or trailing slash (the server's identity too). */
export function normalizePage(raw: string): string {
  try {
    const u = new URL(raw);
    const path = u.pathname.replace(/\/+$/, "") || "/";
    return `${u.protocol}//${u.host.toLowerCase()}${path}`;
  } catch {
    return "";
  }
}

export function boundSelection(selected: string[], maxPages: number, alreadyCaptured = 1): string[] {
  return Array.from(new Set(selected)).slice(0, Math.max(0, maxPages - alreadyCaptured));
}

export function originPattern(url: string): string {
  const u = new URL(url);
  return `${u.protocol}//${u.hostname}/*`;
}
