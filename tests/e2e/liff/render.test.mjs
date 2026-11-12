// [F303-12] Smoke test: the real page loads through the fake server with the fake LIFF SDK injected.
import assert from "node:assert/strict";
import { after, before, test } from "node:test";
import { readCsp, startEnv } from "./lib/harness.mjs";

let env;

before(async () => {
  env = await startEnv();
});

after(async () => {
  await env?.stop();
});

// The one smoke check, used twice: on a normal tab it must pass, on a tab without the SDK stand-in it must fail.
async function smokeCheck(page) {
  await page.goto("/liff");
  assert.equal(
    page.documentHeaders?.["content-security-policy"],
    readCsp(),
    "the page must be served with the product's own Content-Security-Policy, unchanged",
  );
  const hasLiff = await page.eval('typeof window.liff === "object" && typeof window.__e2eLiff === "object"');
  assert.ok(hasLiff, `window.liff is missing: the fake SDK was not injected; blockedRequests=${JSON.stringify(page.blockedRequests)}`);
  assert.equal(
    page.blockedRequests.filter((u) => u.endsWith("/sdk.js")).length,
    0,
    `the SDK request was blocked instead of answered: ${JSON.stringify(page.blockedRequests)}`,
  );
  await page.waitFor('!document.getElementById("gate").hidden', { describe: "consent gate visible" });
  assert.deepEqual(page.cspViolations, [], "the page must raise no Content-Security-Policy violation");
}

test("smoke: real page, real CSP, fake SDK, consent gate shown [F303-12]", async () => {
  const page = await env.newPage();
  try {
    await smokeCheck(page);
  } finally {
    await page.close();
  }
});

test("smoke control: without the SDK stand-in the smoke check goes red [F303-12]", async () => {
  const page = await env.newPage({ fakeSdk: false });
  try {
    await assert.rejects(
      () => smokeCheck(page),
      /window\.liff is missing|SDK request was blocked/,
      "smoke check passed although the SDK interception was off: it would not notice a broken harness",
    );
    assert.ok(
      page.blockedRequests.some((u) => u.endsWith("/sdk.js")),
      `the SDK URL must show up in blockedRequests, got ${JSON.stringify(page.blockedRequests)}`,
    );
  } finally {
    await page.close();
  }
});
