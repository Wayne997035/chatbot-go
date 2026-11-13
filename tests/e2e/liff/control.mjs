// [F303-16] Positive-control runner: proves that each browser test goes red on the version it was written to catch.
//
//   node tests/e2e/liff/control.mjs <commit>        (also: HEAD, whose expected red set is empty)
//   node tests/e2e/liff/control.mjs mutation:<name> (HEAD with the patch listed under that key in controls.json applied)
//   node tests/e2e/liff/control.mjs --verify-report <junit.xml>   (completeness check of a plain run's report)
//
// It builds a throwaway worktree at <commit> under a fresh temp directory, lays the committed tests/e2e/liff of the
// current HEAD on top, stages them there (the hygiene test lists files with `git ls-files`), builds the fake server
// against that version's product code, runs the whole suite, and compares the failed testcases from the JUnit report with
// the list in controls.json. Verdict: only the last line, `CONTROL: PASS` or `CONTROL: FAIL <reason>`, exit code 0 / 1.
//
// [F31-36] A mutation key is for guards whose product code never went wrong in any old commit: it builds the throwaway tree
// at HEAD and applies { file, find, replace } patches to it before the build. The patch only ever touches the throwaway
// tree: `file` must be relative, free of `..` and resolve (symlinks included) inside the tree, and `find` must appear in
// that file exactly once.
//
// Fail-closed: a missing or empty report, skipped or todo tests, a test that should be red but is green, a test that is
// red but not listed, and a red test whose message lacks the listed text (red for another reason) are all FAIL.

import { execFileSync, spawnSync } from "node:child_process";
import { existsSync, mkdtempSync, readFileSync, realpathSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, isAbsolute, join, resolve, sep } from "node:path";
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

// Checks every report must pass, whatever it is compared with. Fail-closed on anything that could hide a missing test.
function structuralProblems(report) {
  const problems = [];
  const { cases, totals } = report;
  if (cases.length === 0) problems.push("the JUnit report holds no testcase");
  for (const key of ["skipped", "todo", "cancelled"]) {
    if (totals[key] === undefined) problems.push(`the JUnit report has no "${key}" total`);
    else if (totals[key] > 0) problems.push(`${totals[key]} ${key} test(s) in the report`);
  }
  if (cases.some((c) => c.skipped)) problems.push("a testcase carries a <skipped> element");
  const seen = new Set();
  for (const c of cases) {
    if (seen.has(c.name)) problems.push(`duplicate testcase name: ${c.name}`);
    seen.add(c.name);
  }
  return problems;
}

// The suite must keep at least these tests; deleting one must not turn the run green unnoticed.
const MIN_TESTCASES = 17;
const REQUIRED_PREFIXES = [
  "smoke:", "T01 ", "T02 ", "T03 ", "T04 ", "T05 ", "T06 ", "T07 ", "T08-quake-on ", "T08-weather-off ", "T08-weather-regions ",
  "T09 ", "T09b ", "T10 ", "T11 ", "T12 ", "hygiene:",
];

// Verdict for a plain run (task e2e:liff): the report is complete and well formed; pass/fail itself comes from node's exit code.
export function verifyReport(report) {
  const problems = structuralProblems(report);
  if (report.cases.length < MIN_TESTCASES) problems.push(`only ${report.cases.length} testcases, at least ${MIN_TESTCASES} are required`);
  for (const prefix of REQUIRED_PREFIXES) {
    if (!report.cases.some((c) => c.name.startsWith(prefix))) problems.push(`no testcase named "${prefix}..." in the report`);
  }
  return problems;
}

// Pure verdict: expected = [{ test, messageIncludes }]. Returns problems (empty = PASS).
export function judge(report, expected) {
  const problems = structuralProblems(report);
  const byName = new Map(report.cases.map((c) => [c.name, c]));
  const wanted = new Map(expected.map((e) => [e.test, e]));
  for (const [name, e] of wanted) {
    const c = byName.get(name);
    if (!c) problems.push(`listed test is not in the report (renamed or removed?): ${name}`);
    else if (!c.failed) problems.push(`should be red but is green (the control did not bite): ${name}`);
    else if (!c.message.includes(e.messageIncludes)) {
      problems.push(`red for another reason: ${name}: expected the failure to mention ${JSON.stringify(e.messageIncludes)}, got ${JSON.stringify(c.message.trim().split("\n")[0])}`);
    }
  }
  for (const c of report.cases) {
    if (c.failed && !wanted.has(c.name)) problems.push(`red but not listed: ${c.name}: ${c.message.trim().split("\n")[0]}`);
  }
  return problems;
}

const MUTATION_PREFIX = "mutation:";

// Returns { mustFail, base, patch }: a commit key is its own base with no patch; a mutation key names its base and patches.
function loadControl(label) {
  const controls = JSON.parse(readFileSync(join(here, "controls.json"), "utf8"));
  const entry = controls[label];
  if (!entry || !Array.isArray(entry.mustFail)) throw new ControlFail(`controls.json has no "mustFail" list for "${label}"`);
  for (const e of entry.mustFail) {
    if (!e.test || !e.messageIncludes) throw new ControlFail(`controls.json entry for "${label}" needs both "test" and "messageIncludes": ${JSON.stringify(e)}`);
  }
  if (!label.startsWith(MUTATION_PREFIX)) return { mustFail: entry.mustFail, base: label, patch: [] };
  if (typeof entry.base !== "string" || entry.base === "") throw new ControlFail(`controls.json entry for "${label}" needs a "base" commit`);
  if (!Array.isArray(entry.patch) || entry.patch.length === 0) throw new ControlFail(`controls.json entry for "${label}" needs a non-empty "patch" list`);
  for (const p of entry.patch) {
    if (typeof p.file !== "string" || p.file === "" || typeof p.find !== "string" || p.find === "" || typeof p.replace !== "string") {
      throw new ControlFail(`controls.json patch for "${label}" needs string "file", "find" and "replace": ${JSON.stringify(p)}`);
    }
  }
  return { mustFail: entry.mustFail, base: entry.base, patch: entry.patch };
}

// The absolute path of a patch target, or a ControlFail when it could leave the throwaway tree.
function patchTarget(tree, file) {
  const escapes = () => new ControlFail(`patch path escapes the throwaway tree: ${JSON.stringify(file)}`);
  if (isAbsolute(file) || file.split(/[\\/]/).includes("..")) throw escapes();
  const target = resolve(tree, file);
  if (!target.startsWith(tree + sep)) throw escapes();
  if (!existsSync(target)) throw new ControlFail(`patch file does not exist in the throwaway tree: ${JSON.stringify(file)}`);
  if (!realpathSync(target).startsWith(tree + sep)) throw escapes(); // a symlink pointing out of the tree
  return target;
}

function applyPatch(tree, patch) {
  for (const p of patch) {
    const target = patchTarget(tree, p.file);
    const text = readFileSync(target, "utf8");
    const count = text.split(p.find).length - 1;
    if (count !== 1) throw new ControlFail(`patch find must appear exactly once in ${p.file}, found ${count}: ${JSON.stringify(p.find)}`);
    writeFileSync(target, text.replace(p.find, () => p.replace));
  }
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
  const { mustFail: expected, base, patch } = loadControl(label);
  const sha = resolveCommit(base);
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
    applyPatch(tree, patch); // [F31-36] mutation keys only; a commit key has an empty patch list
    const tracked = execFileSync("git", ["ls-files", SUITE_DIR], { cwd: tree, encoding: "utf8" }).split("\n").filter(Boolean);
    if (tracked.length === 0) throw new ControlFail("no tracked file under the suite directory in the throwaway tree");
    process.stderr.write(`control: ${label} = ${sha}${patch.length > 0 ? ` + ${patch.length} patch(es)` : ""}; tracked files in the throwaway tree: ${tracked.length}\n`);

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

function verifyReportCli(file) {
  if (!file || !existsSync(file)) {
    console.log(`E2E REPORT: FAIL no JUnit report at ${file ?? "(no path given)"}`);
    return 1;
  }
  const report = parseJunit(readFileSync(file, "utf8"));
  const problems = verifyReport(report);
  if (problems.length > 0) {
    console.log(`E2E REPORT: FAIL ${problems.join("; ")}`);
    return 1;
  }
  console.log(`E2E REPORT: OK ${report.cases.length} testcases, none skipped or todo`);
  return 0;
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const [first, second] = process.argv.slice(2);
  if (first === "--verify-report") {
    process.exit(verifyReportCli(second));
  }
  if (!first) {
    console.log("CONTROL: FAIL usage: node tests/e2e/liff/control.mjs <commit> | --verify-report <junit.xml>");
    process.exit(1);
  }
  try {
    main(first);
    console.log("CONTROL: PASS");
  } catch (err) {
    console.log(`CONTROL: FAIL ${err instanceof ControlFail ? err.message : `unexpected error: ${err.stack}`}`);
    process.exit(1);
  }
}
