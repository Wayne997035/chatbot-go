// [F303-16] Positive-control runner: proves that each browser test goes red on the version it was written to catch.
//
//   node tests/e2e/liff/control.mjs <commit>        (also: HEAD, whose expected red set is empty)
//
// It builds a throwaway worktree at <commit> under a fresh temp directory, lays the committed tests/e2e/liff of the
// current HEAD on top, stages them there (the hygiene test lists files with `git ls-files`), builds the fake server
// against that version's product code, runs the whole suite, and compares the failed testcases from the JUnit report with
// the list in controls.json. Verdict: only the last line, `CONTROL: PASS` or `CONTROL: FAIL <reason>`, exit code 0 / 1.
//
// Fail-closed: a missing or empty report, skipped or todo tests, a test that should be red but is green, a test that is
// red but not listed, and a red test whose message lacks the listed text (red for another reason) are all FAIL.

import { execFileSync, spawnSync } from "node:child_process";
import { existsSync, mkdtempSync, readFileSync, realpathSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve, sep } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const repoRoot = resolve(here, "../../..");
const SUITE_DIR = "tests/e2e/liff";

class ControlFail extends Error {}

function git(args, opts = {}) {
  return execFileSync("git", args, { cwd: repoRoot, encoding: "utf8", maxBuffer: 64 * 1024 * 1024, ...opts });
}

function decodeEntities(text) {
  return text
    .replace(/&#(\d+);/g, (_, n) => String.fromCodePoint(Number(n)))
    .replace(/&lt;/g, "<")
    .replace(/&gt;/g, ">")
    .replace(/&quot;/g, '"')
    .replace(/&apos;/g, "'")
    .replace(/&amp;/g, "&");
}

function attr(attrs, name) {
  const m = new RegExp(`(?:^|\\s)${name}="([^"]*)"`).exec(attrs);
  return m ? decodeEntities(m[1]) : undefined;
}

// Reads node:test's JUnit output. Returns testcases and the run totals node prints as trailing comments.
export function parseJunit(xml) {
  const cases = [];
  for (const m of xml.matchAll(/<testcase\b([^>]*?)(?:\/>|>([\s\S]*?)<\/testcase>)/g)) {
    const name = attr(m[1], "name");
    const inner = m[2] ?? "";
    const failed = attr(m[1], "failure") !== undefined || /<failure\b/.test(inner) || /<error\b/.test(inner);
    const skipped = /<skipped\b/.test(inner);
    const failureTag = /<failure\b([^>]*)>([\s\S]*?)<\/failure>/.exec(inner);
    // node:test escapes the assertion text once before the XML writer escapes it again: decode twice to get the real text.
    const raw = failed ? `${attr(m[1], "failure") ?? ""}\n${failureTag ? attr(failureTag[1], "message") ?? "" : ""}\n${failureTag ? decodeEntities(failureTag[2]) : ""}` : "";
    const message = decodeEntities(raw);
    cases.push({ name, failed, skipped, message });
  }
  const totals = {};
  for (const m of xml.matchAll(/<!-- (tests|suites|pass|fail|cancelled|skipped|todo) (\d+) -->/g)) totals[m[1]] = Number(m[2]);
  return { cases, totals };
}

// Pure verdict: expected = [{ test, messageIncludes }]. Returns problems (empty = PASS) and a per-test table.
export function judge(report, expected) {
  const problems = [];
  const { cases, totals } = report;
  if (cases.length === 0) problems.push("the JUnit report holds no testcase");
  for (const key of ["skipped", "todo", "cancelled"]) {
    if (totals[key] === undefined) problems.push(`the JUnit report has no "${key}" total`);
    else if (totals[key] > 0) problems.push(`${totals[key]} ${key} test(s) in the report`);
  }
  if (cases.some((c) => c.skipped)) problems.push("a testcase carries a <skipped> element");
  const byName = new Map();
  for (const c of cases) {
    if (byName.has(c.name)) problems.push(`duplicate testcase name: ${c.name}`);
    byName.set(c.name, c);
  }
  const wanted = new Map(expected.map((e) => [e.test, e]));
  for (const [name, e] of wanted) {
    const c = byName.get(name);
    if (!c) problems.push(`listed test is not in the report (renamed or removed?): ${name}`);
    else if (!c.failed) problems.push(`should be red but is green (the control did not bite): ${name}`);
    else if (!c.message.includes(e.messageIncludes)) {
      problems.push(`red for another reason: ${name}: expected the failure to mention ${JSON.stringify(e.messageIncludes)}, got ${JSON.stringify(c.message.trim().split("\n")[0])}`);
    }
  }
  for (const c of cases) {
    if (c.failed && !wanted.has(c.name)) problems.push(`red but not listed: ${c.name}: ${c.message.trim().split("\n")[0]}`);
  }
  return problems;
}

function loadExpected(label) {
  const controls = JSON.parse(readFileSync(join(here, "controls.json"), "utf8"));
  const entry = controls[label];
  if (!entry || !Array.isArray(entry.mustFail)) throw new ControlFail(`controls.json has no "mustFail" list for "${label}"`);
  for (const e of entry.mustFail) {
    if (!e.test || !e.messageIncludes) throw new ControlFail(`controls.json entry for "${label}" needs both "test" and "messageIncludes": ${JSON.stringify(e)}`);
  }
  return entry.mustFail;
}

function resolveCommit(label) {
  try {
    return git(["rev-parse", "--verify", `${label}^{commit}`]).trim();
  } catch {
    throw new ControlFail(`cannot resolve "${label}" to a commit in this repository (fetch it first)`);
  }
}

function run(cmd, args, opts) {
  const res = spawnSync(cmd, args, { encoding: "utf8", maxBuffer: 64 * 1024 * 1024, ...opts });
  if (res.error) throw new ControlFail(`${cmd} could not start: ${res.error.message}`);
  return res;
}

function main(label) {
  const expected = loadExpected(label);
  const sha = resolveCommit(label);
  if (git(["status", "--porcelain", "--", SUITE_DIR]).trim() !== "") {
    throw new ControlFail(`${SUITE_DIR} has uncommitted changes; the overlay comes from HEAD, commit first`);
  }
  const tmp = realpathSync(mkdtempSync(join(tmpdir(), "liff-e2e-control-")));
  const tree = join(tmp, "tree");
  let added = false;
  try {
    // The worktree path is created under the fresh temp directory only; removal below checks that again.
    git(["worktree", "add", "--detach", tree, sha]);
    added = true;
    const archive = execFileSync("git", ["archive", "--format=tar", "HEAD", SUITE_DIR], { cwd: repoRoot, maxBuffer: 64 * 1024 * 1024 });
    execFileSync("tar", ["-x", "-C", tree], { input: archive });
    execFileSync("git", ["add", SUITE_DIR], { cwd: tree });
    const tracked = execFileSync("git", ["ls-files", SUITE_DIR], { cwd: tree, encoding: "utf8" }).split("\n").filter(Boolean);
    if (tracked.length === 0) throw new ControlFail("no tracked file under the suite directory in the throwaway tree");
    process.stderr.write(`control: ${label} = ${sha}; tracked files in the throwaway tree: ${tracked.length}\n`);

    const bin = join(tmp, "liff-e2e");
    const build = run("go", ["build", "-o", bin, `./${SUITE_DIR}/server`], { cwd: tree });
    if (build.status !== 0) {
      throw new ControlFail(`the fake server does not build against ${label}:\n${build.stdout}${build.stderr}`);
    }

    const reportFile = join(tmp, "report.xml");
    const files = tracked.filter((f) => /^tests\/e2e\/liff\/[^/]+\.test\.mjs$/.test(f));
    const test = run(
      process.execPath,
      [
        "--test",
        "--test-timeout=60000",
        "--test-reporter=spec",
        "--test-reporter-destination=stderr",
        "--test-reporter=junit",
        `--test-reporter-destination=${reportFile}`,
        ...files,
      ],
      { cwd: tree, env: { ...process.env, LIFF_E2E_SERVER_BIN: bin, LIFF_E2E_REPO_ROOT: tree }, stdio: ["ignore", "inherit", "inherit"] },
    );
    if (!existsSync(reportFile)) throw new ControlFail(`no JUnit report was written (node exited with ${test.status})`);
    const report = parseJunit(readFileSync(reportFile, "utf8"));
    for (const c of report.cases) {
      const why = c.failed ? ` -- ${c.message.trim().split("\n")[0]}` : "";
      process.stderr.write(`control: ${c.failed ? "RED  " : "green"} ${c.name}${why}\n`);
    }
    const problems = judge(report, expected);
    if (problems.length > 0) throw new ControlFail(problems.join("\n  "));
  } finally {
    if (added) {
      if (!tree.startsWith(tmp + sep)) throw new Error(`refusing to remove a worktree outside ${tmp}`);
      git(["worktree", "remove", "--force", tree]);
    }
    if (!tmp.startsWith(realpathSync(tmpdir()))) throw new Error(`refusing to remove ${tmp}`);
    rmSync(tmp, { recursive: true, force: true });
    git(["worktree", "prune"]);
  }
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const label = process.argv[2];
  if (!label) {
    console.log("CONTROL: FAIL usage: node tests/e2e/liff/control.mjs <commit>");
    process.exit(1);
  }
  try {
    main(label);
    console.log("CONTROL: PASS");
  } catch (err) {
    console.log(`CONTROL: FAIL ${err instanceof ControlFail ? err.message : `unexpected error: ${err.stack}`}`);
    process.exit(1);
  }
}
