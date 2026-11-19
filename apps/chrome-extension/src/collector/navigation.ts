import { isExcluded, visibleText } from "../content/dom-sanitizer";

/** Navigation labels plus same-site public routes (paths only, no queries). */
export function collectNavigation(doc: Document, pageUrl: string, redactions: Set<string>): { labels: string[]; routes: string[] } {
  const labels: string[] = [];
  const routes = new Set<string>();
  let origin = "";
  try {
    origin = new URL(pageUrl).origin;
  } catch {
    /* no routes without an origin */
  }
  for (const a of Array.from(doc.querySelectorAll("nav a[href], header a[href], [role='navigation'] a[href]"))) {
    if (isExcluded(a)) continue;
    const label = visibleText(a, redactions, 80);
    if (label && !labels.includes(label)) labels.push(label);
    const href = a.getAttribute("href") ?? "";
    if (origin) {
      try {
        const u = new URL(href, origin);
        if (u.origin === origin && !href.startsWith("//")) {
          if (u.search) redactions.add("route_query_stripped");
          routes.add(u.pathname);
        }
      } catch {
        /* ignore */
      }
    }
    if (labels.length >= 60) break;
  }
  return { labels, routes: Array.from(routes).slice(0, 60) };
}
