import { test } from "node:test";
import assert from "node:assert/strict";
import { decodeClaims, lensTokenProblem, normalizeApiBase } from "../src/auth/session";

const b64 = (o: object) => Buffer.from(JSON.stringify(o)).toString("base64url");
const jwt = (c: object) => `${b64({ alg: "RS256" })}.${b64(c)}.sig`;
const now = Math.floor(Date.now() / 1000);

test("only short-lived, tenant-pinned lens tokens are accepted for storage", () => {
  const good = jwt({ sub: "a", tid: "t", scope: "lens", iat: now, exp: now + 900 });
  assert.equal(lensTokenProblem(good), null);
  assert.equal(decodeClaims(good)?.tid, "t");
  assert.match(lensTokenProblem(jwt({ sub: "a", tid: "t", iat: now, exp: now + 900 }))!, /lens-scoped/);
  assert.match(lensTokenProblem(jwt({ scope: "lens", iat: now, exp: now + 900 }))!, /tenant/);
  assert.match(lensTokenProblem(jwt({ scope: "lens", tid: "t", iat: now, exp: now + 7200 }))!, /one hour/);
  assert.match(lensTokenProblem(jwt({ scope: "lens", tid: "t", iat: now - 1000, exp: now - 10 }))!, /expired/);
  assert.match(lensTokenProblem("not-a-jwt")!, /JWT/);
});

test("the API must be https (http only for localhost)", () => {
  assert.equal(normalizeApiBase("https://api.example.com/v1/x"), "https://api.example.com");
  assert.equal(normalizeApiBase("http://localhost:8080"), "http://localhost:8080");
  assert.throws(() => normalizeApiBase("http://api.example.com"));
});
