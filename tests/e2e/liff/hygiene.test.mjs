// [F303-16] Repo hygiene: nothing under tests/e2e/liff may carry a local path, an internal tool name or a key.
// The path and key patterns are assembled from pieces so this file never matches its own scan. The two private words
// (an internal tool name, a local account name) are stored only as sha256 [F31-23]: the public repo holds no text
// that spells them, and the scan compares hashes of what it finds in each line.
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
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
  { label: "private key block", re: new RegExp(`${"BEGIN"} .*${"PRIVATE"} ${"KEY"}`) },
  { label: "real JWT prefix", re: new RegExp(`${"Bearer"} ${"ey"}`) },
];

// [F31-23] sha256 of each private word (lower case) and its length. Hashing hides the words from readers of the repo;
// it is not encryption. The length lets the scan hash every window of that length inside a line.
export const PRIVATE_TOKEN_SHA256 = [
  { label: "internal browser tool", length: 12, sha256: "b3e0eca10ecaf4e09baa18d53e16cea04194347634111a7758fcbd4cb643ce85" },
  { label: "local account name", length: 9, sha256: "d900edfb203b76fc81cb4c5b3e5c14cfff71dac0cf51b93a6ec505464240d700" },
];

function sha256Hex(text) {
  return createHash("sha256").update(text).digest("hex");
}

// Labels of the private words found in one line. Every window of each listed length in the lower-cased line is hashed,
// so a word is found whole, glued to letters or digits ("xxword", "word42"), or inside a hyphenated compound: a match
// is a window of the line, and windows cover everything that taking maximal [a-z] / [a-z0-9] runs and "-" joined
// neighbours of them would yield.
function privateWordsIn(line, hashes) {
  const lower = line.toLowerCase();
  const labels = [];
  for (const { label, length, sha256 } of hashes) {
    for (let i = 0; i + length <= lower.length; i++) {
      if (sha256Hex(lower.slice(i, i + length)) === sha256) {
        labels.push(label);
        break;
      }
    }
  }
  return labels;
}

// Pure scan of (name, text) pairs; returns one finding per forbidden pattern per line. hashes is injectable so the
// control test can prove the hash path with made-up words instead of the real ones.
export function scan(files, hashes = PRIVATE_TOKEN_SHA256) {
  const findings = [];
  for (const { name, text } of files) {
    text.split("\n").forEach((line, i) => {
      for (const { label, re } of FORBIDDEN) {
        if (re.test(line)) findings.push(`${name}:${i + 1}: ${label}`);
      }
      for (const label of privateWordsIn(line, hashes)) findings.push(`${name}:${i + 1}: ${label}`);
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
    `-----${"BEGIN"} RSA ${"PRIVATE"} ${"KEY"}-----`,
    `${"Bearer"} ${"ey"}JhbGciOi`,
  ];
  assert.equal(samples.length, FORBIDDEN.length, "every forbidden pattern needs a sample");
  samples.forEach((text, i) => {
    const found = scan([{ name: "in-memory", text }]);
    assert.equal(found.length, 1, `pattern "${FORBIDDEN[i].label}" did not fire on its sample: ${JSON.stringify(found)}`);
    assert.ok(found[0].endsWith(FORBIDDEN[i].label), `wrong pattern fired: ${found[0]}`);
  });
  // The hash path: made-up words with the injected hashes (the real words never appear in this repo).
  const made = [
    { label: "made-up tool", word: "zebra-quux" },
    { label: "made-up name", word: "plover" },
  ].map(({ label, word }) => ({ label, length: word.length, sha256: sha256Hex(word) }));
  made.forEach(({ label }, i) => {
    const text = ["zebra-quux", "PLOVER"][i];
    assert.deepEqual(scan([{ name: "in-memory", text }], made), [`in-memory:1: ${label}`]);
  });
  assert.deepEqual(scan([{ name: "clean", text: "const ok = 1;\nconst fake = 'e2e-fake-id-token';" }]), []);
  assert.throws(() => requireFiles([]), /no tracked files/);
});

test("hygiene: private words are matched by hash, also inside longer words [F31-23]", () => {
  const made = [
    { label: "made-up tool", word: "zebra-quux" },
    { label: "made-up name", word: "plover" },
  ].map(({ label, word }) => ({ label, length: word.length, sha256: sha256Hex(word) }));
  const hit = (text) => scan([{ name: "s", text }], made).map((f) => f.replace(/^s:\d+: /, ""));
  // Letters glued on, digits glued on, and a hyphenated compound around the word, for the plain word and the "-" word.
  assert.deepEqual(hit("xxploverxx"), ["made-up name"], "letters on both sides");
  assert.deepEqual(hit("plover42"), ["made-up name"], "digits after");
  assert.deepEqual(hit("7plover"), ["made-up name"], "digit before");
  assert.deepEqual(hit("my-plover-nest"), ["made-up name"], "inside a hyphenated compound");
  assert.deepEqual(hit("xzebra-quuxy"), ["made-up tool"], "hyphen word with letters glued on");
  assert.deepEqual(hit("a1-zebra-quux-2b"), ["made-up tool"], "hyphen word inside a longer hyphenated name");
  assert.deepEqual(hit("ZeBrA-QuUx"), ["made-up tool"], "case does not matter");
  // Near misses must stay quiet: the hash is of the exact word.
  assert.deepEqual(hit("plove ver, zebra quux, zebra_quux, zebraquux"), []);
  // The real list: well formed, and no entry is a placeholder.
  for (const { length, sha256 } of PRIVATE_TOKEN_SHA256) {
    assert.match(sha256, /^[0-9a-f]{64}$/);
    assert.ok(Number.isInteger(length) && length >= 6, "a very short word would match innocent text");
  }
});
