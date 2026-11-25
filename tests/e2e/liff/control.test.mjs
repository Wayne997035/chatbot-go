// [F37-15] The control runner's own refusals, run without Chrome or the fake server. Fixtures are built under the temp
// directory at run time: a symlink that leaves a tree cannot be committed sensibly.
import assert from "node:assert/strict";
import { mkdirSync, mkdtempSync, readFileSync, realpathSync, rmSync, symlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import { ControlFail, loadControl, patchTarget } from "./control.mjs";

const here = dirname(fileURLToPath(import.meta.url));
const MUST_FAIL = [{ test: "T01 some test", messageIncludes: "some text" }];

function refusal(fn) {
  try {
    fn();
  } catch (err) {
    return err;
  }
  return null;
}

// A tree and a sibling directory outside it, both under one real temp directory.
function withFixture(body) {
  const tmp = realpathSync(mkdtempSync(join(tmpdir(), "liff-ctl-test-")));
  const tree = join(tmp, "tree");
  const outside = join(tmp, "outside");
  mkdirSync(tree);
  mkdirSync(outside);
  try {
    return body({ tree, outside });
  } finally {
    rmSync(tmp, { recursive: true, force: true });
  }
}

test("control: a commit key may not carry patch or base [F37-15]", () => {
  for (const [field, value] of [["patch", [{ file: "a", find: "b", replace: "c" }]], ["base", "HEAD"]]) {
    const err = refusal(() => loadControl("c7ac93b", { c7ac93b: { mustFail: MUST_FAIL, [field]: value } }));
    assert.notEqual(err, null, `a commit key carrying "${field}" did not throw`);
    assert.ok(err instanceof ControlFail, `a commit key carrying "${field}" threw something other than ControlFail: ${err}`);
    assert.ok(err.message.includes(`"${field}"`) && err.message.includes("c7ac93b"), `the refusal must name the key and the field "${field}": ${err.message}`);
  }
  // Positive pair: a plain commit key still loads, so "always throws" cannot pass.
  assert.deepEqual(loadControl("c7ac93b", { c7ac93b: { mustFail: MUST_FAIL } }), { mustFail: MUST_FAIL, base: "c7ac93b", patch: [] });
  // Every key in the committed controls.json still loads.
  const real = JSON.parse(readFileSync(join(here, "controls.json"), "utf8"));
  for (const key of Object.keys(real)) {
    const err = refusal(() => loadControl(key, real));
    assert.equal(err, null, `the committed entry "${key}" is refused: ${err?.message}`);
  }
  assert.ok(Object.keys(real).includes("HEAD"), "the committed controls.json has no HEAD entry");
});

test("control: patchTarget refuses a symlink that leaves the tree [F37-15]", () => {
  withFixture(({ tree, outside }) => {
    writeFileSync(join(outside, "target.txt"), "outside\n");
    writeFileSync(join(tree, "real.txt"), "inside\n");
    symlinkSync(outside, join(tree, "link"));
    symlinkSync(join(tree, "real.txt"), join(tree, "inlink.txt"));

    const err = refusal(() => patchTarget(tree, "link/target.txt"));
    assert.notEqual(err, null, "a patch path through a symlink that leaves the tree was accepted");
    assert.ok(err instanceof ControlFail && /escapes the throwaway tree/.test(err.message), `wrong refusal for the symlink escape: ${err}`);
    for (const file of ["../outside/target.txt", join(outside, "target.txt")]) {
      const bad = refusal(() => patchTarget(tree, file));
      assert.ok(bad instanceof ControlFail && /escapes the throwaway tree/.test(bad.message), `${file} was not refused as an escape: ${bad}`);
    }
    // Positive pair: a plain file and a symlink that stays inside the tree are accepted.
    assert.equal(patchTarget(tree, "real.txt"), join(tree, "real.txt"));
    assert.equal(patchTarget(tree, "inlink.txt"), join(tree, "inlink.txt"));
  });
});

test("control: patchTarget refuses a patch file that is missing [F37-15]", () => {
  withFixture(({ tree }) => {
    const err = refusal(() => patchTarget(tree, "missing.txt"));
    assert.notEqual(err, null, "a patch file that does not exist was accepted");
    assert.ok(err instanceof ControlFail && /does not exist in the throwaway tree/.test(err.message), `wrong refusal for the missing file: ${err}`);
    // Positive pair: once the file exists the same call returns its path.
    writeFileSync(join(tree, "missing.txt"), "now here\n");
    assert.equal(patchTarget(tree, "missing.txt"), join(tree, "missing.txt"));
  });
});
