import { isExcluded, sanitizeText, visibleText } from "../content/dom-sanitizer";

/** Labels of visible interactive features (buttons, search, tabs). */
export function collectVisibleFeatures(doc: Document, redactions: Set<string>): string[] {
  const out: string[] = [];
  for (const el of Array.from(doc.querySelectorAll("button, [role='button'], [role='search'], [role='tab'], input[type='submit'], input[type='search']"))) {
    if (isExcluded(el)) continue;
    let t = visibleText(el, redactions, 80);
    if (!t) t = sanitizeText(el.getAttribute("aria-label") ?? el.getAttribute("title") ?? "", redactions, 80);
    if (!t && el.getAttribute("role") === "search") t = "Search";
    if (!t && el.getAttribute("type") === "search") t = "Search";
    if (t && !out.includes(t)) out.push(t);
    if (out.length >= 60) break;
  }
  return out;
}
