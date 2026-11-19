import { sanitizeText, sanitizeUrl } from "../content/dom-sanitizer";

const PUBLIC_META = ["description", "keywords", "generator", "application-name", "robots", "og:title", "og:type", "og:site_name", "og:description", "twitter:card"];

/** Public, page-level metadata only (no csrf tokens, user ids or other meta). */
export function collectPublicMetadata(doc: Document, redactions: Set<string>): Record<string, string> {
  const out: Record<string, string> = {};
  for (const key of PUBLIC_META) {
    const el = doc.querySelector(`meta[name='${key}'], meta[property='${key}']`);
    const v = el?.getAttribute("content");
    if (v) out[key] = sanitizeText(v, redactions, 300);
  }
  const lang = doc.documentElement.getAttribute("lang");
  if (lang) out.lang = lang.slice(0, 16);
  const canonical = doc.querySelector("link[rel='canonical']")?.getAttribute("href");
  if (canonical) {
    const c = sanitizeUrl(new URL(canonical, doc.baseURI || "https://invalid.invalid/").toString(), redactions);
    if (c) out.canonical = c;
  }
  return out;
}
