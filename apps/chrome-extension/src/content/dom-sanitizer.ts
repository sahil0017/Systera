// Client-side sanitizer and exclusion rules (§7.3). The server re-applies all of this
// (browserintel.SanitizeObservation); the client does it first so excluded data never leaves
// the browser. Collectors read text and attributes only: never form values, cookies,
// storage, history, request bodies or hidden content.

// Non-global patterns: a /g regex keeps lastIndex between .test() calls and silently skips
// matches on the next string.
const SECRET_PATTERNS: RegExp[] = [
  /bearer\s+[a-z0-9._~+/-]{12,}=*/i,
  /eyJ[a-zA-Z0-9_-]{10,}\.[a-zA-Z0-9_-]{10,}\.[a-zA-Z0-9_-]{10,}/,
  /\b(?:sk|pk|rk|gsk|ghp|gho|xox[abpr]|AKIA|AIza)[-_]?[a-zA-Z0-9_-]{12,}\b/,
  /\b(?:password|passwd|pwd|cookie|set-cookie|secret|api[_-]?key|token|session(?:id)?)\s*[:=]\s*\S+/i,
  /\b(?:\d[ -]?){13,19}\b/,
  /\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b/,
  /\b\d{3}-\d{2}-\d{4}\b/,
  /(?:\+\d{1,3}[ .-]?)?\(?\d{3}\)?[ .-]\d{3}[ .-]\d{4}\b/,
];

export const SENSITIVE_QUERY_KEYS = new Set([
  "token", "access_token", "id_token", "refresh_token", "auth", "code", "key", "api_key", "apikey", "secret",
  "client_secret", "signature", "sig", "password", "pwd", "pass", "session", "sessionid", "sid", "cookie", "jwt",
  "email", "phone", "otp", "state", "nonce",
]);

export const MAX_TEXT = 300;

export function sanitizeText(raw: string, redactions: Set<string>, max = MAX_TEXT): string {
  let out = raw.replace(/\s+/g, " ").trim();
  for (const re of SECRET_PATTERNS) {
    if (re.test(out)) {
      out = out.replace(new RegExp(re.source, re.flags + "g"), "[REDACTED]");
      redactions.add("client_secret_or_pii_redacted");
    }
  }
  if (out.length > max) out = Array.from(out).slice(0, max).join("");
  return out;
}

/** Strips credentials, fragments and sensitive query parameters; "" if not http(s). */
export function sanitizeUrl(rawUrl: string, redactions: Set<string>): string {
  try {
    const u = new URL(rawUrl);
    if (u.protocol !== "http:" && u.protocol !== "https:") return "";
    if (u.username || u.password) {
      u.username = "";
      u.password = "";
      redactions.add("url_credentials_stripped");
    }
    if (u.hash) {
      u.hash = "";
      redactions.add("url_fragment_stripped");
    }
    for (const key of Array.from(u.searchParams.keys())) {
      if (SENSITIVE_QUERY_KEYS.has(key.toLowerCase())) {
        u.searchParams.delete(key);
        redactions.add("url_sensitive_params_stripped");
      }
    }
    return u.toString();
  } catch {
    return "";
  }
}

/** Origin + path only: resource URLs routinely carry tokens in their query strings. */
export function resourceUrl(rawUrl: string, redactions: Set<string>): string {
  try {
    const u = new URL(rawUrl);
    if (u.protocol !== "http:" && u.protocol !== "https:") return "";
    if (u.search || u.hash || u.username) redactions.add("resource_query_stripped");
    return u.origin + u.pathname;
  } catch {
    return "";
  }
}

// Containers that typically hold personal values (account menus, profile cards, billing).
const PERSONAL_CONTAINER = /(^|[\s_-])(account|profile|user-?menu|avatar|billing|payment|wallet|address|my-?orders)([\s_-]|$)/i;

/**
 * The single exclusion rule every collector applies: hidden, aria-hidden, template/noscript,
 * editable, explicitly private, or inside a personal-data container.
 */
export function isExcluded(el: Element): boolean {
  const win = el.ownerDocument.defaultView;
  for (let cur: Element | null = el; cur; cur = cur.parentElement) {
    const tag = cur.tagName.toLowerCase();
    if (tag === "template" || tag === "noscript" || tag === "script" || tag === "style") return true;
    if (tag === "textarea" || tag === "select") return true;
    if (cur.hasAttribute("hidden") || cur.getAttribute("aria-hidden") === "true") return true;
    if (cur.getAttribute("contenteditable") === "true" || cur.getAttribute("contenteditable") === "") return true;
    if (cur.hasAttribute("data-private") || cur.hasAttribute("data-sensitive")) return true;
    const ident = `${cur.id} ${typeof cur.className === "string" ? cur.className : ""}`;
    if (tag !== "body" && tag !== "html" && PERSONAL_CONTAINER.test(ident)) return true;
    if (win) {
      const st = win.getComputedStyle(cur);
      if (st.display === "none" || st.visibility === "hidden" || st.opacity === "0") return true;
    }
  }
  return false;
}

/** Visible, non-excluded text of an element (textContent; never input values). */
export function visibleText(el: Element, redactions: Set<string>, max = MAX_TEXT): string {
  if (isExcluded(el)) return "";
  return sanitizeText(el.textContent ?? "", redactions, max);
}
