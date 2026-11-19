import { test } from "node:test";
import assert from "node:assert/strict";
import { JSDOM } from "jsdom";
import { buildNormalizedObservation } from "../src/browser-adapter/normalize";
import { sanitizeText } from "../src/content/dom-sanitizer";

const PAGE = `<!doctype html><html lang="en"><head>
  <title>Acme Shop — signed in as jane.doe@example.com</title>
  <meta name="description" content="Buy widgets">
  <meta name="csrf-token" content="CSRF-SECRET-123">
  <meta property="og:site_name" content="Acme">
  <meta name="user-id" content="USER-42">
  <link rel="canonical" href="https://shop.example.com/home?sid=SESSION-999#x">
</head><body>
  <header><nav>
    <a href="/search?q=private+query">Search</a>
    <a href="/cart">Cart</a>
    <a href="https://evil.example.net/phish">External</a>
    <a href="//evil.example.net/x">Protocol relative</a>
    <a href="/pricing#plans">Pricing</a>
    <div class="account-menu"><a href="/account">Jane Doe</a><span>Balance $1,234</span></div>
  </nav></header>
  <main>
    <h1>Widgets for teams</h1>
    <h2 style="display:none">Hidden admin heading</h2>
    <h2 hidden>Also hidden</h2>
    <div aria-hidden="true"><h2>Aria hidden heading</h2></div>
    <template><h2>Template heading</h2></template>
    <p>Our catalogue ships worldwide in two days.</p>
    <p>Call us on +1 415-555-0100 for bulk orders today.</p>
    <p data-private>Your order 7781 ships to 1 Main St.</p>
    <div contenteditable="true"><p>Draft note: my diary entry here</p></div>
    <section id="user-profile"><p>Jane Doe, member since 2019, loyalty tier gold</p></section>
    <button>Checkout</button>
    <button style="visibility:hidden">Secret button</button>
    <div role="search"><input type="search" placeholder="Search products"></div>
    <form action="https://shop.example.com/login?next=/account&token=TOKEN-ABC" method="post">
      <label for="u">Email address</label><input id="u" name="email" value="jane.doe@example.com">
      <label for="p">Password</label><input id="p" type="password" name="password" value="hunter2">
      <input type="hidden" name="csrf" value="CSRF-HIDDEN-456">
      <input name="cc-number" autocomplete="cc-number" placeholder="Card number" value="4111111111111111">
      <textarea name="notes">typed private notes</textarea>
      <button type="submit">Sign in</button>
    </form>
    <script>window.secret = "SCRIPT-SECRET"</script>
  </main>
</body></html>`;

function observe(html = PAGE, url = "https://user:pw@shop.example.com/home?session=SESS-1&ref=nav#frag") {
  const dom = new JSDOM(html, { url: "https://shop.example.com/home" });
  const input = dom.window.document.querySelector("#u") as HTMLInputElement;
  input.value = "typed-after-load@example.com"; // a live value the collector must never read
  const perf = {
    getEntriesByType: () => [
      { name: "https://cdn.example.com/app.js?v=3&signature=SIG-777", initiatorType: "script", duration: 12.4 },
      { name: "https://api.example.com/v1/me?access_token=AT-888", initiatorType: "fetch", duration: 40 },
      { name: "data:image/png;base64,AAAA", initiatorType: "img", duration: 1 },
    ],
  } as unknown as Performance;
  return buildNormalizedObservation(dom.window.document, url, perf);
}

test("collects the allowed structure", () => {
  const o = observe();
  assert.equal(o.url, "https://shop.example.com/home?ref=nav");
  assert.ok(o.observations.headings.includes("Widgets for teams"));
  assert.deepEqual(o.observations.navigation.slice(0, 2), ["Search", "Cart"]);
  assert.deepEqual(o.observations.public_routes.sort(), ["/cart", "/pricing", "/search"]);
  assert.ok(o.observations.visible_features.includes("Checkout"));
  assert.ok(o.observations.visible_features.includes("Search"));
  assert.ok(o.observations.visible_text.includes("Our catalogue ships worldwide in two days."));
  assert.equal(o.observations.forms[0].action, "/login");
  assert.equal(o.observations.forms[0].method, "POST");
  assert.ok(o.observations.forms[0].labels.includes("Email address"));
  assert.equal(o.observations.public_metadata.description, "Buy widgets");
  assert.equal(o.observations.public_metadata.canonical, "https://shop.example.com/home");
  assert.equal(o.observations.public_metadata.lang, "en");
  assert.deepEqual(o.observations.resource_metadata.map((r) => r.name), ["https://cdn.example.com/app.js", "https://api.example.com/v1/me"]);
});

test("never collects excluded data", () => {
  const all = JSON.stringify(observe());
  for (const leaked of [
    "jane.doe@example.com", "typed-after-load", "hunter2", "CSRF-SECRET", "CSRF-HIDDEN", "USER-42", "SESSION-999", "SESS-1", "pw@",
    "TOKEN-ABC", "4111111111111111", "Card number", "typed private notes", "SCRIPT-SECRET", "SIG-777", "AT-888", "private+query",
    "Hidden admin heading", "Also hidden", "Aria hidden heading", "Template heading", "Secret button", "1 Main St", "diary entry",
    "Jane Doe", "Balance", "loyalty tier", "415-555-0100", "evil.example.net", "#frag", "data:image",
  ]) {
    assert.ok(!all.includes(leaked), `leaked: ${leaked}`);
  }
  const red = observe().redactions_applied;
  for (const r of ["client_secret_or_pii_redacted", "sensitive_form_field_skipped", "url_sensitive_params_stripped", "url_credentials_stripped", "resource_query_stripped", "route_query_stripped"]) {
    assert.ok(red.includes(r), `missing redaction label ${r}: ${red}`);
  }
});

test("secret patterns redact consistently across calls (no global-regex lastIndex skips)", () => {
  const r = new Set<string>();
  for (let i = 0; i < 4; i++) {
    assert.equal(sanitizeText("mail a@b.io now", r), "mail [REDACTED] now");
  }
});
