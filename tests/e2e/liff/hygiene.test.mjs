// [F303-16] Repo hygiene: nothing under tests/e2e/liff may carry a local path, an internal tool name or a key.
// The forbidden strings are assembled from pieces so this file never matches its own scan.
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { test } from "node:test";
import { repoRoot } from "./lib/harness.mjs";

const FORBIDDEN = [
  { label: "user home path", re: new RegExp(`/${"Us"}ers/`) },
  { label: "linux home path", re: new RegExp(`/${"ho"}me/`) },
  { label: "private temp path", re: new RegExp(`/${"pri"}vate/`) },
  { label: "scratch directory", re: new RegExp(`${"scratch"}pad`) },
  { label: "assistant config directory", re: new RegExp(`\\.${"cla"}ude`) },
  { label: "internal browser tool", re: new RegExp(`${"headless"}-${"cdp"}`) },
  { label: "local account name", re: new RegExp(`${"wayne"}${"chen"}`) },
  { label: "private key block", re: new RegExp(`${"BEGIN"} .*${"PRIVATE"} ${"KEY"}`) },
  { label: "real JWT prefix", re: new RegExp(`${"Bearer"} ${"ey"}`) },
];

// Pure scan of (name, text) pairs; returns one finding per forbidden pattern per line.
export function scan(files) {
  const findings = [];
  for (const { name, text } of files) {
    text.split("\n").forEach((line, i) => {
      for (const { label, re } of FORBIDDEN) {
        if (re.test(line)) findings.push(`${name}:${i + 1}: ${label}`);
      }
    });
  }
  return findings;
}

// The tracked file list; an empty list is an error (a scan over nothing proves nothing).
export function requireFiles(list) {
  if (list.length === 0) throw new Error("no tracked files under tests/e2e/liff: the hygiene scan would check nothing");
  return list;
}

function trackedFiles() {
  const out = execFileSync("git", ["ls-files", "tests/e2e/liff"], { cwd: repoRoot, encoding: "utf8" });
  return requireFiles(out.split("\n").filter(Boolean));
}

test("hygiene: tracked files under tests/e2e/liff are free of local paths, tool names and keys [F303-16]", () => {
  const names = trackedFiles();
  const files = names.map((name) => ({ name, text: readFileSync(join(repoRoot, name), "utf8") }));
  assert.deepEqual(scan(files), []);
});

test("hygiene control: the scan reports every forbidden pattern and an empty file list is an error [F303-16]", () => {
  const samples = [
    `const p = "/${"Us"}ers/x/file";`,
    `const p = "/${"ho"}me/x/file";`,
    `const p = "/${"pri"}vate/tmp/x";`,
    `see ${"scratch"}pad notes`,
    `require("../.${"cla"}ude/x")`,
    `${"headless"}-${"cdp"}.mjs`,
    `${"wayne"}${"chen"}`,
    `-----${"BEGIN"} RSA ${"PRIVATE"} ${"KEY"}-----`,
    `${"Bearer"} ${"ey"}JhbGciOi`,
  ];
  assert.equal(samples.length, FORBIDDEN.length, "every forbidden pattern needs a sample");
  samples.forEach((text, i) => {
    const found = scan([{ name: "in-memory", text }]);
    assert.equal(found.length, 1, `pattern "${FORBIDDEN[i].label}" did not fire on its sample: ${JSON.stringify(found)}`);
    assert.ok(found[0].endsWith(FORBIDDEN[i].label), `wrong pattern fired: ${found[0]}`);
  });
  assert.deepEqual(scan([{ name: "clean", text: "const ok = 1;\nconst fake = 'e2e-fake-id-token';" }]), []);
  assert.throws(() => requireFiles([]), /no tracked files/);
});
