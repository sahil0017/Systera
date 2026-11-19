import { test } from "node:test";
import assert from "node:assert/strict";
import { applyRedactions, payloadBytes, previewItems } from "../src/shared/redaction";
import { boundSelection, candidatePages, normalizePage, originPattern } from "../src/shared/pages";
import { NormalizedObservation } from "../src/shared/types";

const obs: NormalizedObservation = {
  schema_version: "1.0", collector_version: "1.1.0", url: "https://shop.example.com/", title: "Shop", user_initiated: true,
  observations: {
    headings: ["A", "B"], navigation: ["Cart"], visible_features: [], visible_text: ["t1"],
    forms: [{ action: "/login", method: "POST", labels: ["Email", "Password"] }], public_routes: ["/cart", "/pricing/", "/cart", "//x.com/a", "/"],
    public_metadata: { description: "d", generator: "Gen" }, resource_metadata: [{ name: "https://cdn/x.js" }],
  },
  redactions_applied: [],
};

test("every previewed item can be removed before sending", () => {
  const items = previewItems(obs);
  const ids = items.map((i) => i.id);
  for (const id of ["title", "headings.1", "forms.0", "forms.0.labels.1", "public_metadata.generator", "resource_metadata.0"]) assert.ok(ids.includes(id), id);
  const out = applyRedactions(obs, new Set(["headings.0", "forms.0.labels.1", "public_metadata.generator", "resource_metadata.0", "title"]));
  assert.deepEqual(out.observations.headings, ["B"]);
  assert.deepEqual(out.observations.forms[0].labels, ["Email"]);
  assert.deepEqual(Object.keys(out.observations.public_metadata), ["description"]);
  assert.equal(out.observations.resource_metadata.length, 0);
  assert.equal(out.title, "");
  assert.ok(out.redactions_applied.includes("user_redacted_items"));
  assert.equal(applyRedactions(obs, new Set(["forms.0"])).observations.forms.length, 0);
  assert.equal(obs.observations.headings.length, 2, "original untouched");
  assert.ok(payloadBytes(out) < payloadBytes(obs));
  assert.equal(applyRedactions(obs, new Set()), obs);
});

test("multi-page candidates stay on the origin and are bounded", () => {
  assert.deepEqual(candidatePages("https://shop.example.com/", obs.observations.public_routes), ["https://shop.example.com/cart", "https://shop.example.com/pricing"]);
  assert.equal(normalizePage("https://Shop.example.com/a/?q=1#x"), "https://shop.example.com/a");
  assert.equal(boundSelection(["a", "b", "c", "a"], 3).join(), "a,b");
  assert.equal(originPattern("https://shop.example.com/x?y"), "https://shop.example.com/*");
});
