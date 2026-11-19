import { visibleText } from "../content/dom-sanitizer";

export function collectHeadings(doc: Document, redactions: Set<string>): string[] {
  const out: string[] = [];
  for (const el of Array.from(doc.querySelectorAll("h1, h2, h3"))) {
    const t = visibleText(el, redactions);
    if (t && !out.includes(t)) out.push(t);
    if (out.length >= 50) break;
  }
  return out;
}
