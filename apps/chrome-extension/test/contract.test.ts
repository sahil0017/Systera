import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { JSDOM } from "jsdom";
import { buildNormalizedObservation } from "../src/browser-adapter/normalize";

// The extension's payload must use exactly the JSON names the Go server decodes.
function goTags(structName: string): string[] {
  const src = readFileSync("../../control-plane/internal/browserintel/browserintel.go", "utf8");
  const body = src.split(`type ${structName} struct {`)[1].split("\n}")[0];
  return Array.from(body.matchAll(/json:"([a-z_]+)/g)).map((m) => m[1]);
}

test("observation payload matches browserintel.ObservationPayload / ObservationInput", () => {
  const dom = new JSDOM("<title>x</title><h1>Hi</h1>", { url: "https://example.com/" });
  const o = buildNormalizedObservation(dom.window.document, "https://example.com/");
  assert.deepEqual(Object.keys(o.observations).sort(), goTags("ObservationPayload").sort());
  const input = goTags("ObservationInput");
  for (const k of Object.keys(o)) assert.ok(input.includes(k), `server does not accept ${k}`);
});
