// [F31-20] [F31-21] Cleanup guarantees of the e2e harness: however a test process ends (normally, with an error, or
// by a signal) it leaves neither a Chrome process group nor the Go fake server behind.
//
// Every scenario runs in a child node process that drives the real harness against a fake Chrome script (written to a
// temp dir; it speaks just enough of the DevTools WebSocket handshake). The child prints the pids it spawned and this
// file checks only those pids: other test files run in parallel and have their own Chrome and servers.
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { chmodSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { constants as osConstants, tmpdir } from "node:os";
import { join } from "node:path";
import { after, before, test } from "node:test";
import { sleep } from "./lib/cdp.mjs";

const harnessUrl = new URL("./lib/harness.mjs", import.meta.url).href;
// The child kills itself via SIGKILL at this limit: well under `node --test --test-timeout=60000`.
const CHILD_LIMIT_MS = 25000;

// A stand-in for Chrome. FAKE_CHROME_MODE: "silent" accepts the WebSocket but never answers, "answer" replies to
// Browser.getVersion, "refuse" announces a DevTools port nobody listens on. A helper process joins the same process
// group, like Chrome's own helper processes; both pids are written to FAKE_CHROME_PIDFILE.
const FAKE_CHROME = `
const http = require("node:http");
const net = require("node:net");
const crypto = require("node:crypto");
const fs = require("node:fs");
const { spawn } = require("node:child_process");
const mode = process.env.FAKE_CHROME_MODE;
const helper = spawn(process.execPath, ["-e", "setInterval(() => {}, 1000)"], { stdio: "ignore" });
fs.writeFileSync(process.env.FAKE_CHROME_PIDFILE, JSON.stringify({ chrome: process.pid, helper: helper.pid }));
setInterval(() => {}, 1000);
function announce(port) {
  process.stderr.write("DevTools listening on ws://127.0.0.1:" + port + "/devtools/browser/fake\\n");
}
function frame(obj) {
  const body = Buffer.from(JSON.stringify(obj));
  return Buffer.concat([Buffer.from([0x81, body.length]), body]);
}
function onData(socket, chunk) {
  const len = chunk[1] & 0x7f;
  const mask = chunk.subarray(2, 6);
  const payload = Buffer.from(chunk.subarray(6, 6 + len)).map((b, i) => b ^ mask[i % 4]);
  const msg = JSON.parse(payload.toString());
  if (msg.method === "Browser.getVersion") socket.write(frame({ id: msg.id, result: { product: "FakeChrome/1" } }));
}
if (mode === "refuse") {
  const probe = net.createServer();
  probe.listen(0, "127.0.0.1", () => {
    const port = probe.address().port;
    probe.close(() => announce(port));
  });
} else {
  const server = http.createServer();
  server.on("upgrade", (req, socket) => {
    const accept = crypto.createHash("sha1").update(req.headers["sec-websocket-key"] + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11").digest("base64");
    socket.write("HTTP/1.1 101 Switching Protocols\\r\\nUpgrade: websocket\\r\\nConnection: Upgrade\\r\\nSec-WebSocket-Accept: " + accept + "\\r\\n\\r\\n");
    socket.on("error", () => {});
    if (mode === "answer") socket.on("data", (chunk) => onData(socket, chunk));
  });
  server.listen(0, "127.0.0.1", () => announce(server.address().port));
}
`;

// The child process: one scenario per run, pids reported through the harness's own spawn hook.
const CHILD = `
const h = await import(process.env.HARNESS_URL);
const t = h._cleanupForTest;
t.hooks.onSpawn = (kind, pid) => console.log("PID " + kind + " " + pid);
const scenario = process.env.SCENARIO;
if (scenario === "start-chrome") {
  try {
    await t.startChrome(process.env.LIFF_E2E_CHROME);
    console.log("UNEXPECTED started");
  } catch (err) {
    console.log("THREW " + err.message.split("\\n")[0]);
  }
} else if (scenario === "env") {
  await h.startEnv();
  console.log("READY");
  setInterval(() => {}, 1000);
} else if (scenario === "server") {
  await t.startServer();
  console.log("READY");
  setInterval(() => {}, 1000);
}
`;

let dir;
let fakeChrome;
let pidfile;
before(() => {
  dir = mkdtempSync(join(tmpdir(), "liff-e2e-cleanup-"));
  fakeChrome = join(dir, "fake-chrome");
  pidfile = join(dir, "pids.json");
  writeFileSync(fakeChrome, `#!${process.execPath}\n${FAKE_CHROME}`);
  chmodSync(fakeChrome, 0o755);
});
after(() => rmSync(dir, { recursive: true, force: true }));

function isAlive(pid) {
  try {
    process.kill(pid, 0);
    return true;
  } catch (err) {
    if (err.code === "ESRCH") return false;
    if (err.code === "EPERM") return true;
    throw err;
  }
}

// The pids that are still alive after waiting up to ms for them to go.
async function survivors(pids, ms = 3000) {
  const deadline = Date.now() + ms;
  for (;;) {
    const alive = pids.filter(isAlive);
    if (alive.length === 0 || Date.now() > deadline) return alive;
    await sleep(50);
  }
}

// Whatever a failed assertion leaves behind is ended here, so a red test never poisons the next one.
function reap(run, pids) {
  run.child.kill("SIGKILL");
  for (const pid of pids) {
    for (const target of [-pid, pid]) {
      try {
        process.kill(target, "SIGKILL");
      } catch (err) {
        if (err.code !== "ESRCH" && err.code !== "EPERM") throw err;
      }
    }
  }
}

function launch(scenario, env) {
  const child = spawn(process.execPath, ["--input-type=module", "-e", CHILD], {
    env: { ...process.env, SCENARIO: scenario, HARNESS_URL: harnessUrl, LIFF_E2E_CHROME: fakeChrome, FAKE_CHROME_PIDFILE: pidfile, ...env },
    stdio: ["ignore", "pipe", "pipe"],
  });
  const run = { child, out: "", err: "", backstop: false, startedAt: Date.now() };
  child.stdout.setEncoding("utf8");
  child.stderr.setEncoding("utf8");
  child.stdout.on("data", (c) => (run.out += c));
  child.stderr.on("data", (c) => (run.err += c));
  const timer = setTimeout(() => {
    run.backstop = true;
    child.kill("SIGKILL");
  }, CHILD_LIMIT_MS);
  run.closed = new Promise((resolve) =>
    child.once("close", (code, signal) => {
      clearTimeout(timer);
      run.elapsedMs = Date.now() - run.startedAt;
      resolve({ code, signal });
    }),
  );
  return run;
}

// The pids the child reported, plus the fake Chrome's helper process from its pid file.
function reportedPids(run) {
  const pids = [...run.out.matchAll(/^PID (\w+) (\d+)$/gm)].map((m) => ({ kind: m[1], pid: Number(m[2]) }));
  try {
    pids.push({ kind: "chrome-helper", pid: JSON.parse(readFileSync(pidfile, "utf8")).helper });
  } catch {
    // No pid file: the fake Chrome never started; the assertion on the reported pids says so.
  }
  return pids;
}

async function waitReady(run) {
  let exited = false;
  run.closed.then(() => (exited = true));
  while (!/^READY$/m.test(run.out)) {
    assert.ok(!exited, `the child exited before READY. stdout:\n${run.out}\nstderr:\n${run.err}`);
    await sleep(25);
  }
}

function assertNoSurvivors(alive, pids, why) {
  const names = alive.map((pid) => `${pids.find((p) => p.pid === pid)?.kind} ${pid}`);
  assert.deepEqual(names, [], `${why}: processes still alive: ${names.join(", ")}`);
}

test("cleanup: Chrome and its profile are removed when the DevTools connection fails [F31-20]", async () => {
  const run = launch("start-chrome", { FAKE_CHROME_MODE: "refuse" });
  let pids = [];
  try {
    await run.closed;
    pids = reportedPids(run);
    assert.ok(pids.some((p) => p.kind === "chrome"), "the child reported no Chrome pid");
    assertNoSurvivors(await survivors(pids.map((p) => p.pid)), pids, `after Cdp.connect failed (child stuck until SIGKILL: ${run.backstop})`);
    assert.ok(!run.backstop, `the child hung after the connection failed (stuck until SIGKILL). stdout:\n${run.out}`);
    assert.match(run.out, /^THREW cannot connect/m, `expected a connection error. stdout:\n${run.out}\nstderr:\n${run.err}`);
  } finally {
    reap(run, pids.map((p) => p.pid));
  }
});

test("cleanup: an unanswered Browser.getVersion still cleans up [F31-20]", async () => {
  const run = launch("start-chrome", { FAKE_CHROME_MODE: "silent" });
  let pids = [];
  try {
    await run.closed;
    pids = reportedPids(run);
    assert.ok(pids.some((p) => p.kind === "chrome"), "the child reported no Chrome pid");
    assertNoSurvivors(await survivors(pids.map((p) => p.pid)), pids, `after Browser.getVersion went unanswered (child stuck until SIGKILL: ${run.backstop})`);
    assert.ok(!run.backstop, `the child hung on an unanswered Browser.getVersion (stuck until SIGKILL). stdout:\n${run.out}`);
    assert.ok(run.elapsedMs < 15000, `startChrome took ${run.elapsedMs}ms to give up on Browser.getVersion`);
    assert.match(run.out, /^THREW Browser\.getVersion: no answer within \d+ms$/m, `stdout:\n${run.out}\nstderr:\n${run.err}`);
  } finally {
    reap(run, pids.map((p) => p.pid));
  }
});

// A signal ends the test process through the harness's handlers; the exit code is the conventional 128 + signal.
async function signalScenario(scenario, signal) {
  const run = launch(scenario, { FAKE_CHROME_MODE: "answer" });
  let pids = [];
  try {
    await waitReady(run);
    pids = reportedPids(run);
    assert.ok(pids.some((p) => p.kind === "server"), `the child reported no fake server pid. stdout:\n${run.out}`);
    run.child.kill(signal);
    const { code } = await run.closed;
    assert.ok(!run.backstop, `the child did not end on ${signal} (stuck until SIGKILL)`);
    assert.equal(code, 128 + osConstants.signals[signal], `exit code after ${signal}`);
    assertNoSurvivors(await survivors(pids.map((p) => p.pid)), pids, `after ${signal}`);
    return pids;
  } finally {
    reap(run, pids.map((p) => p.pid));
  }
}

test("cleanup: SIGTERM ends Chrome and the fake server [F31-21]", async () => {
  const pids = await signalScenario("env", "SIGTERM");
  assert.ok(pids.some((p) => p.kind === "chrome"), "the child reported no Chrome pid");
});

test("cleanup: SIGINT ends Chrome and the fake server [F31-21]", async () => {
  const pids = await signalScenario("env", "SIGINT");
  assert.ok(pids.some((p) => p.kind === "chrome"), "the child reported no Chrome pid");
});

test("cleanup: a signal before Chrome starts still ends the fake server [F31-21]", async () => {
  const pids = await signalScenario("server", "SIGTERM");
  assert.ok(!pids.some((p) => p.kind === "chrome"), "this scenario must not start Chrome");
});
