import { test } from "node:test";
import assert from "node:assert/strict";
import { applyLearning, classifyTab, expectedCapabilities, groupPreview, looksPersonal, pairCapabilities, personalKind, priorInvestigations, rankPages, suggestMode, suggestedPages, summarizeEvidence } from "../src/sidepanel/smart";
import { parseRefinement, summarize } from "../src/sidepanel/ondevice";
import { WEBSITES_TTL_MS, loadCachedWebsites, saveCachedWebsites } from "../src/sidepanel/cache";
import type { NormalizedObservation, WebsiteProfile } from "../src/shared/types";

const obs = (nav: string[]): NormalizedObservation => ({
  schema_version: "1.0", collector_version: "t", url: "https://shop.example.com/", title: "Shop", user_initiated: true, redactions_applied: [],
  observations: { headings: [], navigation: nav, visible_features: [], visible_text: [], forms: [], public_routes: [], public_metadata: {}, resource_metadata: [] },
});

test("classifyTab recognises scannable, restricted and unknown tabs", () => {
  assert.deepEqual(classifyTab({ url: "https://www.example.com/x" }), { kind: "ready", host: "example.com", origin: "https://www.example.com" });
  assert.equal(classifyTab({ url: "chrome://settings" }).kind, "restricted");
  assert.equal(classifyTab({ url: "https://chromewebstore.google.com/detail/x" }).kind, "restricted");
  assert.equal(classifyTab({}).kind, "unknown");
});

test("rankPages matches whole path segments, skips locales and collapses detail templates", () => {
  const base = "https://shop.example.com";
  const ranked = rankPages([`${base}/about`, `${base}/blog/post`, `${base}/cart`, `${base}/pricing`, `${base}/checkout`, `${base}/login`,
    `${base}/en-us/checkout-now`, `${base}/news`, `${base}/products/123`, `${base}/products/456`]);
  assert.equal(ranked[0].path, "/checkout");
  assert.deepEqual(new Set(ranked.slice(0, 5).map((p) => p.path)), new Set(["/checkout", "/en-us/checkout-now", "/cart", "/pricing", "/login"]));
  assert.equal(ranked.find((p) => p.path === "/news")!.tag, "content", "/news is content, not 'new'");
  assert.equal(ranked.find((p) => p.path === "/en-us/checkout-now")!.tag, "checkout", "locale prefix skipped, checkout-now matched");
  assert.equal(ranked.filter((p) => p.template === "/products/:id").length, 1, "detail pages collapse to one template");
  assert.ok(ranked.at(-1)!.score < 0);
});

test("suggestedPages takes one page of each kind before repeats", () => {
  const base = "https://shop.example.com";
  const picks = suggestedPages([`${base}/checkout`, `${base}/payment`, `${base}/cart`, `${base}/pricing`], 3);
  assert.equal(picks.length, 3);
  assert.deepEqual(rankPages(picks).map((p) => p.tag).sort(), ["cart", "checkout", "pricing"], "one checkout-type page, then cart and pricing");
});

test("suggestMode explains itself and scales with what the page exposes", () => {
  const base = "https://shop.example.com";
  const q = suggestMode(null, [], 0);
  assert.equal(q.mode, "quick_scan");
  assert.equal(suggestMode(obs(["Home"]), [`${base}/about`], 0).mode, "quick_scan");
  const pm = suggestMode(obs(["Home"]), [`${base}/cart`, `${base}/pricing`], 0);
  assert.equal(pm.mode, "product_map");
  assert.ok(pm.signals.includes("/cart linked") && pm.confidence > 0 && pm.source === "rules");
  const full = suggestMode(obs(["Cart", "Sign in", "Pricing", "Search", "Products"]), [`${base}/cart`, `${base}/pricing`, `${base}/login`, `${base}/search`], 0);
  assert.equal(full.mode, "full_system_design");
  assert.equal(suggestMode(obs(["Cart", "Search"]), [`${base}/cart`, `${base}/pricing`], 2).mode, "compare_websites");
});

test("expected capabilities use the server's taxonomy keywords and passive payment clues", () => {
  const o = obs(["Search", "Sign in", "Basket"]);
  o.observations.resource_metadata = [{ name: "https://js.stripe.com/v3" }];
  assert.deepEqual(expectedCapabilities(o).map((c) => c.key).sort(), ["auth", "cart", "catalogue", "checkout", "search"]);
  assert.ok(!expectedCapabilities(obs(["Research"])).some((c) => c.key === "search"), "word boundaries: 'Research' is not search");
});

test("learning follows the user's own overrides, and only after enough of them", () => {
  const base = suggestMode(obs(["Home"]), ["https://s.com/cart", "https://s.com/pricing"], 0);
  assert.equal(applyLearning(base, { product_map: { full_system_design: 2 } }).mode, "product_map", "2 overrides are not enough");
  const learned = applyLearning(base, { product_map: { full_system_design: 4, product_map: 1 } });
  assert.equal(learned.mode, "full_system_design");
  assert.equal(learned.source, "learned");
  assert.match(learned.reason, /4 of 5 times/);
});

test("on-device model output is validated strictly and page text is bounded", () => {
  const pages = rankPages(["https://s.com/cart", "https://s.com/pricing"]);
  const ok = parseRefinement(JSON.stringify({ mode: "product_map", reason: "Cart and pricing pages.", pages: [0, 1, 1, 9] }), pages, 0);
  assert.deepEqual(ok?.pages, ["https://s.com/cart", "https://s.com/pricing"]);
  assert.equal(ok?.source, "on-device");
  assert.equal(parseRefinement("not json", pages, 0), null);
  assert.equal(parseRefinement(JSON.stringify({ mode: "hack", reason: "x", pages: [] }), pages, 0), null);
  assert.equal(parseRefinement(JSON.stringify({ mode: "compare_websites", reason: "x", pages: [] }), pages, 0), null, "nothing to compare");
  assert.equal(parseRefinement(JSON.stringify({ mode: "quick_scan", reason: "", pages: [] }), pages, 0), null);
  const long = obs(Array.from({ length: 50 }, (_, i) => "Nav ".repeat(40) + i));
  assert.ok(summarize(long, pages, 0).length < 3000);
});

test("the investigation list cache is per workspace and expires", async () => {
  const store: Record<string, unknown> = {};
  (globalThis as Record<string, unknown>).chrome = { storage: { session: {
    get: async (k: string[]) => ({ [k[0]]: store[k[0]] }), set: async (v: Record<string, unknown>) => Object.assign(store, v) } } };
  const items = [{ id: "w1", domain: "a.com" } as WebsiteProfile];
  await saveCachedWebsites("https://api", items, 1000);
  assert.equal((await loadCachedWebsites("https://api", 2000))?.[0].id, "w1");
  assert.equal(await loadCachedWebsites("https://other", 2000), null);
  assert.equal(await loadCachedWebsites("https://api", 1000 + WEBSITES_TTL_MS + 1), null);
});

test("preview groups use readable labels and flag personal-looking text", () => {
  const groups = groupPreview([
    { id: "visible_features.0", section: "visible_features", text: "Add to cart" },
    { id: "title", section: "title", text: "Shop" },
    { id: "resource_metadata.0", section: "resource timing", text: "https://js.stripe.com/v3" },
    { id: "headings.0", section: "headings", text: "" },
  ]);
  assert.deepEqual(groups.map((g) => g.label), ["Page title", "Visible features", "Loaded scripts & assets"]);
  assert.ok(looksPersonal("Welcome back, Priya"));
  assert.equal(personalKind("contact jane@acme.io"), "email");
  assert.equal(personalKind("Call +1 415 555 0100"), "phone");
  assert.equal(personalKind("Visa •••• 4242"), "card");
  assert.equal(personalKind("Order #A1B2C3"), "order");
  assert.equal(personalKind("12 Baker Street"), "address");
  for (const fine of ["Free shipping over $50", "Sign in", "Popular products", "Welcome to our store", "Search 2,000 items"]) {
    assert.equal(personalKind(fine), null, fine);
  }
});

test("findings are summarised and paired with the component proposed for them", () => {
  const caps = [
    { name: "Shopping Cart", evidence_class: "OBSERVED" as const, evidence_refs: [], confidence: "HIGH", rationale: "" },
    { name: "Cart Service", evidence_class: "PROPOSED" as const, evidence_refs: [], confidence: "MEDIUM", rationale: "Proposed logical component supporting the observed Shopping Cart capability; not observed directly." },
  ];
  assert.deepEqual(summarizeEvidence(caps), { OBSERVED: 1, INFERRED: 0, PROPOSED: 1 });
  assert.equal(pairCapabilities(caps)[0].component?.name, "Cart Service");
  const w = (id: string, domain: string, created_at: string) => ({ id, domain, created_at } as WebsiteProfile);
  assert.deepEqual(priorInvestigations([w("a", "shop.example.com", "2026-01-01"), w("b", "www.shop.example.com", "2026-02-01"), w("c", "other.com", "2026-03-01")], "shop.example.com").map((x) => x.id), ["b", "a"]);
});
