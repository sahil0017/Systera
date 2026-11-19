import { visibleText } from "../content/dom-sanitizer";

/** Short visible text blocks from the main content (not chrome, forms or personal areas). */
export function collectVisibleText(doc: Document, redactions: Set<string>): string[] {
  const out: string[] = [];
  const root = doc.querySelector("main, [role='main'], article") ?? doc.body;
  if (!root) return out;
  for (const el of Array.from(root.querySelectorAll("p, li, dt, dd, figcaption, blockquote"))) {
    if (el.closest("nav, header, footer, form, [role='navigation']")) continue;
    const t = visibleText(el, redactions, 200);
    if (t.length >= 12 && !out.includes(t)) out.push(t);
    if (out.length >= 40) break;
  }
  return out;
}
