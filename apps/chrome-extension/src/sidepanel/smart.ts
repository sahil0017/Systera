// Context-aware helpers for the side panel. Pure functions over data Lens already has (the
// active tab, the preview the user is about to approve, findings returned by the workspace):
// nothing here collects anything new.
import { PreviewItem } from "../shared/redaction";
import { AnalysisMode, CapabilityFinding, NormalizedObservation, WebsiteProfile } from "../shared/types";

export interface TabContext {
  id?: number;
  url?: string;
  title?: string;
  favIconUrl?: string;
}

export type TabState =
  | { kind: "unknown" } // Lens cannot see the tab (no activeTab grant yet)
  | { kind: "restricted"; reason: string } // chrome://, Web Store, file://...
  | { kind: "ready"; host: string; origin: string };

export function classifyTab(t: TabContext): TabState {
  if (!t.url) return { kind: "unknown" };
  let u: URL;
  try {
    u = new URL(t.url);
  } catch {
    return { kind: "unknown" };
  }
  if (u.protocol !== "https:" && u.protocol !== "http:") {
    return { kind: "restricted", reason: `${u.protocol.replace(":", "")}:// pages can't be scanned. Open a regular website.` };
  }
  if (u.hostname === "chromewebstore.google.com" || (u.hostname === "chrome.google.com" && u.pathname.startsWith("/webstore"))) {
    return { kind: "restricted", reason: "Chrome Web Store pages can't be scanned." };
  }
  return { kind: "ready", host: u.hostname.replace(/^www\./, ""), origin: u.origin };
}

export const MODE_INFO: Record<AnalysisMode, { label: string; short: string; detail: string; agents: number; multiPage: boolean }> = {
  quick_scan: { label: "Quick scan", short: "This page", detail: "Capabilities and technology signals from this page.", agents: 3, multiPage: false },
  product_map: { label: "Product map", short: "Pages you pick", detail: "Capabilities, user journeys and domains across pages you choose.", agents: 5, multiPage: true },
  full_system_design: { label: "System design", short: "Pages you pick", detail: "All nine specialists: HLD, data, APIs, infrastructure and an independent review.", agents: 9, multiPage: true },
  compare_websites: { label: "Compare", short: "Then pick sites", detail: "Investigate this site, then compare it with your other investigations.", agents: 5, multiPage: true },
};

export const MODES: AnalysisMode[] = ["quick_scan", "product_map", "full_system_design", "compare_websites"];

/** Why Lens suggests a mode: a confidence and the concrete signals behind it. */
export interface ModeSuggestion {
  mode: AnalysisMode;
  reason: string;
  confidence: number; // 0..1
  signals: string[]; // e.g. "Cart visible", "/checkout linked"
  source: "rules" | "on-device" | "learned";
}

/**
 * The capability list the server classifies against (control-plane CapabilityTaxonomy,
 * capability-taxonomy/1), so what Lens expects before sending matches what comes back.
 */
export const TAXONOMY: Array<{ key: string; name: string; keywords: string[] }> = [
  { key: "search", name: "Search", keywords: ["search"] },
  { key: "cart", name: "Cart", keywords: ["cart", "basket"] },
  { key: "checkout", name: "Checkout & payments", keywords: ["checkout", "payment"] },
  { key: "auth", name: "Sign-in", keywords: ["login", "log in", "sign in", "sign up", "register"] },
  { key: "catalogue", name: "Catalogue", keywords: ["catalog", "catalogue", "products", "shop"] },
  { key: "notifications", name: "Subscriptions", keywords: ["subscribe", "newsletter"] },
  { key: "upload", name: "Uploads", keywords: ["upload"] },
  { key: "pricing", name: "Plans & pricing", keywords: ["pricing", "plans"] },
];

const PAYMENT_CLUES = /(js\.stripe\.com|checkout\.stripe|paypal\.com|braintree|adyen|razorpay|klarna|checkout\.com)/i;

function wordRe(kw: string): RegExp {
  return new RegExp(`(^|[^a-z0-9])${kw.replace(/[.*+?^${}()|[\]\\]/g, "\\$&").replace(/ /g, "\\s*")}([^a-z0-9]|$)`, "i");
}
const TAXONOMY_RES = TAXONOMY.map((t) => ({ ...t, res: t.keywords.map(wordRe) }));

export interface ExpectedCapability {
  key: string;
  name: string;
  hits: number; // how many visible elements mention it
}

/** Capabilities the server is likely to report, from the same visible text it will see. */
export function expectedCapabilities(obs: NormalizedObservation): ExpectedCapability[] {
  const o = obs.observations;
  const texts = [obs.title, ...o.headings, ...o.navigation, ...o.visible_features, ...o.visible_text, ...o.forms.flatMap((f) => f.labels)];
  const out: ExpectedCapability[] = [];
  for (const t of TAXONOMY_RES) {
    const hits = texts.filter((x) => t.res.some((re) => re.test(x))).length;
    if (hits > 0) out.push({ key: t.key, name: t.name, hits });
  }
  if (!out.some((c) => c.key === "checkout") && o.resource_metadata.some((r) => PAYMENT_CLUES.test(r.name))) {
    out.push({ key: "checkout", name: "Checkout & payments", hits: 1 });
  }
  return out.sort((a, b) => b.hits - a.hits);
}

/**
 * Suggests a mode from the preview. Rules weigh how much product the page exposes (expected
 * capabilities) against how much more there is to see (key pages linked from it).
 */
export function suggestMode(obs: NormalizedObservation | null, candidates: string[], priorCount: number): ModeSuggestion {
  if (!obs) return { mode: "quick_scan", reason: "Start with a quick scan of this page.", confidence: 0.5, signals: [], source: "rules" };
  const caps = expectedCapabilities(obs);
  const key = rankPages(candidates).filter((p) => p.score > 0);
  const keyTags = [...new Set(key.map((p) => p.tag).filter(Boolean))] as string[];
  const signals = [
    ...caps.slice(0, 3).map((c) => `${c.name} visible`),
    ...key.slice(0, 3).map((p) => `${p.path} linked`),
  ];
  const breadth = caps.length + keyTags.length; // distinct things there are to learn
  if (priorCount > 0 && key.length >= 2 && caps.length >= 2) {
    return { mode: "compare_websites", confidence: 0.6, signals, source: "rules",
      reason: `You have ${priorCount} other investigation${priorCount === 1 ? "" : "s"}; map a few pages and compare.` };
  }
  if (breadth >= 6 && key.length >= 3) {
    return { mode: "full_system_design", confidence: Math.min(0.9, 0.5 + breadth * 0.05), signals, source: "rules",
      reason: `A full product: ${caps.length} capabilities here and ${key.length} key pages (${keyTags.slice(0, 3).join(", ")}).` };
  }
  if (key.length >= 2) {
    return { mode: "product_map", confidence: Math.min(0.85, 0.45 + key.length * 0.08), signals, source: "rules",
      reason: `${key.length} key pages (${keyTags.slice(0, 3).join(", ")}) are linked from here and worth mapping.` };
  }
  return { mode: "quick_scan", confidence: caps.length > 0 ? 0.7 : 0.55, signals, source: "rules",
    reason: caps.length > 0 ? `This page shows ${caps.map((c) => c.name.toLowerCase()).slice(0, 3).join(", ")}; few other key pages are linked.` : "This page alone covers what is visible." };
}

// Path segments (whole words) that reveal architecture, strongest first.
const SEGMENT_WEIGHTS: Array<[string[], number, string]> = [
  [["checkout", "payment", "payments", "pay"], 10, "checkout"],
  [["cart", "basket", "bag"], 9, "cart"],
  [["pricing", "plans", "plan", "billing", "subscribe", "subscription"], 8, "pricing"],
  [["login", "log-in", "signin", "sign-in", "signup", "sign-up", "register", "account", "auth", "join"], 7, "account"],
  [["search", "find"], 6, "search"],
  [["products", "product", "catalog", "catalogue", "shop", "store", "collections", "collection", "category", "categories"], 5, "catalogue"],
  [["upload", "create", "new", "editor", "dashboard", "app"], 4, "app"],
  [["docs", "api", "developers", "developer", "integrations"], 3, "developer"],
  [["about", "blog", "news", "careers", "jobs", "press", "legal", "privacy", "terms", "cookies", "help", "faq", "contact"], -3, "content"],
];
const LOCALE = /^[a-z]{2}([-_][a-z]{2,4})?$/i;
const ID_LIKE = /^(\d+|[0-9a-f]{8,}|[0-9a-f-]{20,}|.*\d{4,}.*)$/i;

export interface RankedPage {
  url: string;
  path: string;
  score: number;
  tag?: string;
  template: string; // /products/:id
}

/**
 * Ranks candidate pages so the ones that reveal the most architecture come first. Detail
 * pages that share a template (/products/123, /products/456) are collapsed to one.
 */
export function rankPages(urls: string[]): RankedPage[] {
  const seen = new Set<string>();
  const out: RankedPage[] = [];
  for (const url of urls) {
    let path = url;
    try {
      path = new URL(url).pathname || "/";
    } catch {
      /* keep url */
    }
    const segs = path.toLowerCase().split("/").filter(Boolean);
    const meaningful = segs.filter((s, i) => !(i === 0 && LOCALE.test(s) && segs.length > 1));
    const template = "/" + meaningful.map((s) => (ID_LIKE.test(s) ? ":id" : s)).join("/");
    if (seen.has(template)) continue;
    seen.add(template);
    let score = 0;
    let tag: string | undefined;
    for (const [words, w, t] of SEGMENT_WEIGHTS) {
      if (meaningful.some((s) => words.includes(s) || words.some((x) => s.startsWith(x + "-")))) {
        if (tag === undefined || w > score) {
          score = w;
          tag = t;
        }
      }
    }
    if (template.includes(":id")) score -= 2; // one detail page tells as much as many
    score -= Math.max(0, meaningful.length - 2);
    out.push({ url, path, score, tag, template });
  }
  return out.sort((a, b) => b.score - a.score || a.path.length - b.path.length || a.path.localeCompare(b.path));
}

/** The best few pages to capture next: one per kind of page first, then the rest. */
export function suggestedPages(urls: string[], limit: number): string[] {
  const ranked = rankPages(urls).filter((p) => p.score > 0);
  const firstOfTag = ranked.filter((p, i) => ranked.findIndex((q) => q.tag === p.tag) === i);
  const rest = ranked.filter((p) => !firstOfTag.includes(p));
  return [...firstOfTag, ...rest].slice(0, Math.max(0, limit)).map((p) => p.url);
}

// ---------------------------------------------------------------------------
// Learning from the user's own overrides. Only counts per (suggested -> chosen) mode pair are
// kept, in extension storage: never site names or page content.
// ---------------------------------------------------------------------------

export type ChoiceStats = Partial<Record<AnalysisMode, Partial<Record<AnalysisMode, number>>>>;
const STATS_KEY = "lens_mode_choices";

export async function loadChoiceStats(): Promise<ChoiceStats> {
  try {
    const r = await chrome.storage?.local?.get?.(STATS_KEY);
    return ((r as Record<string, unknown> | undefined)?.[STATS_KEY] as ChoiceStats) ?? {};
  } catch {
    return {};
  }
}

export async function recordChoice(stats: ChoiceStats, suggested: AnalysisMode, chosen: AnalysisMode): Promise<ChoiceStats> {
  const next: ChoiceStats = { ...stats, [suggested]: { ...(stats[suggested] ?? {}) } };
  const row = next[suggested]!;
  row[chosen] = Math.min(50, (row[chosen] ?? 0) + 1);
  try {
    await chrome.storage?.local?.set?.({ [STATS_KEY]: next });
  } catch {
    /* storage unavailable: learning is best-effort */
  }
  return next;
}

/** If the user usually replaces this suggestion with another mode, suggest that instead. */
export function applyLearning(s: ModeSuggestion, stats: ChoiceStats): ModeSuggestion {
  const row = stats[s.mode];
  if (!row) return s;
  const total = Object.values(row).reduce((n, v) => n + (v ?? 0), 0);
  const [best, count] = (Object.entries(row) as Array<[AnalysisMode, number]>).sort((a, b) => b[1] - a[1])[0] ?? [];
  if (!best || best === s.mode || total < 3 || count / total < 0.6) return s;
  return { ...s, mode: best, source: "learned", confidence: Math.min(0.9, count / total),
    reason: `You usually pick ${MODE_INFO[best].label} when Lens suggests ${MODE_INFO[s.mode].label} (${count} of ${total} times).` };
}

export const SECTION_LABELS: Record<string, string> = {
  title: "Page title",
  headings: "Headings",
  navigation: "Navigation",
  visible_features: "Visible features",
  visible_text: "Visible text",
  public_routes: "Links on this site",
  forms: "Forms (labels only)",
  "form labels": "Form labels",
  "public metadata": "Public metadata",
  "resource timing": "Loaded scripts & assets",
};

export interface PreviewGroup {
  section: string;
  label: string;
  items: PreviewItem[];
}

export function groupPreview(items: PreviewItem[]): PreviewGroup[] {
  const order = Object.keys(SECTION_LABELS);
  const map = new Map<string, PreviewItem[]>();
  for (const i of items) {
    if (!i.text) continue;
    map.set(i.section, [...(map.get(i.section) ?? []), i]);
  }
  const rank = (s: string) => (order.indexOf(s) < 0 ? 99 : order.indexOf(s));
  return [...map.entries()]
    .sort(([a], [b]) => rank(a) - rank(b))
    .map(([section, xs]) => ({ section, label: SECTION_LABELS[section] ?? section, items: xs }));
}

/**
 * Text that looks personal even after Lens's own redaction: worth a second look before
 * sending. Errs on the side of flagging; the user decides.
 */
const PERSONAL_PATTERNS: Array<[RegExp, string]> = [
  [/[a-z0-9._%+-]+@[a-z0-9-]+(\.[a-z0-9-]+)*\.[a-z]{2,}/i, "email"],
  [/(\+\d{1,3}[\s.-]?)?(\(?\d{3}\)?[\s.-]?)\d{3}[\s.-]?\d{4}\b/, "phone"],
  [/\b([Hh]i|[Hh]ello|[Hh]ey|[Ww]elcome back|[Ss]igned in as|[Ll]ogged in as),?\s+[A-Z][a-z]+/, "name"],
  [/(•{2,}|\*{4}|x{4})\s?\d{4}\b/i, "card"],
  [/\b(order|invoice|booking|ticket)\s*(#|no\.?|number)\s*[a-z0-9-]{4,}/i, "order"],
  [/\b\d{1,5}\s+[A-Z][a-z]+(\s[A-Z][a-z]+)*\s(Street|St|Avenue|Ave|Road|Rd|Lane|Ln|Drive|Dr|Boulevard|Blvd)\b/, "address"],
];

export function personalKind(text: string): string | null {
  for (const [re, kind] of PERSONAL_PATTERNS) if (re.test(text)) return kind;
  return null;
}

export function looksPersonal(text: string): boolean {
  return personalKind(text) !== null;
}

export interface EvidenceSummary {
  OBSERVED: number;
  INFERRED: number;
  PROPOSED: number;
}

export function summarizeEvidence(xs: Array<{ evidence_class: string }>): EvidenceSummary {
  const out: EvidenceSummary = { OBSERVED: 0, INFERRED: 0, PROPOSED: 0 };
  for (const x of xs) if (x.evidence_class in out) out[x.evidence_class as keyof EvidenceSummary]++;
  return out;
}

export interface CapabilityPair {
  capability: CapabilityFinding;
  component?: CapabilityFinding;
}

/** Pairs each observed capability with the component proposed to support it. */
export function pairCapabilities(caps: CapabilityFinding[]): CapabilityPair[] {
  const out: CapabilityPair[] = [];
  const proposed = caps.filter((c) => c.evidence_class === "PROPOSED");
  for (const c of caps.filter((x) => x.evidence_class !== "PROPOSED")) {
    const comp = proposed.find((p) => p.rationale.includes(`observed ${c.name} capability`));
    out.push({ capability: c, component: comp });
  }
  for (const p of proposed) if (!out.some((o) => o.component === p)) out.push({ capability: p });
  return out;
}

/** Previous investigations of the same site, newest first. */
export function priorInvestigations(all: WebsiteProfile[], host: string): WebsiteProfile[] {
  const h = host.replace(/^www\./, "");
  return all
    .filter((w) => w.domain.replace(/^www\./, "") === h)
    .sort((a, b) => (b.created_at ?? "").localeCompare(a.created_at ?? ""));
}

export function timeAgo(iso: string | undefined, now = Date.now()): string {
  if (!iso) return "";
  const s = Math.max(0, Math.round((now - new Date(iso).getTime()) / 1000));
  if (s < 60) return "just now";
  if (s < 3600) return `${Math.round(s / 60)} min ago`;
  if (s < 86400) return `${Math.round(s / 3600)} h ago`;
  return `${Math.round(s / 86400)} d ago`;
}
