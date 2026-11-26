// [F40-12] [F40-13] [F40-14] Chrome-free tests of the bounds in the e2e harness: every wait on a browser, a DevTools
// socket or a fake-server reply ends within a limit and names what did not answer.
//
// Each test carries its own Promise.race guard so that a missing bound shows up as a named failure instead of a
// node:test timeout (a test-level timeout would count as cancelled and fail the judge for the wrong reason).
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { chmodSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import http from "node:http";
import net from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { Cdp, sleep, waitFor } from "./lib/cdp.mjs";
import * as harness from "./lib/harness.mjs";

// Resolves with the value of promise, or rejects with guardMessage when ms pass first.
function guarded(promise, ms, guardMessage) {
  let timer;
  const guard = new Promise((_, reject) => {
    timer = setTimeout(() => reject(new Error(guardMessage)), ms);
  });
  return Promise.race([promise, guard]).finally(() => clearTimeout(timer));
}

// The rejection of a promise, or a failure when it resolves; a guard message passes through as the failure.
async function rejection(promise, ms, guardMessage) {
  let outcome;
  try {
    outcome = { value: await guarded(promise, ms, guardMessage) };
  } catch (err) {
    if (err.message === guardMessage) throw err;
    return err;
  }
  throw new Error(`expected a rejection, got ${JSON.stringify(outcome.value)}`);
}

// A WebSocket stand-in: readyState, send(), close() and addEventListener(); emit() plays a server frame or a close.
function fakeSocket({ readyState = 1 } = {}) {
  const listeners = { message: [], close: [] };
  return {
    readyState,
    sent: [],
    send(data) {
      this.sent.push(data);
    },
    close() {},
    addEventListener(type, fn) {
      (listeners[type] ??= []).push(fn);
    },
    emit(type, ev) {
      for (const fn of listeners[type] ?? []) fn(ev);
    },
  };
}

test("cdp: waitFor rejects when one poll never returns [F40-12]", async () => {
  const started = Date.now();
  const err = await rejection(
    waitFor(() => new Promise(() => {}), { timeout: 300, describe: "never-polled" }),
    800,
    "waitFor did not return within timeout+500ms",
  );
  const took = Date.now() - started;
  assert.ok(took < 800, `waitFor took ${took}ms`);
  const order = ["timed out after 300ms waiting for never-polled", "poll call did not return", "last value:"];
  let from = 0;
  for (const part of order) {
    const at = err.message.indexOf(part, from);
    assert.ok(at >= 0, `message lacks "${part}" in order: ${err.message}`);
    from = at + part.length;
  }
});

test("cdp: waitFor keeps polling falsy and throwing polls [F40-12]", async () => {
  let calls = 0;
  assert.equal(
    await waitFor(() => (++calls <= 2 ? 0 : "ok"), { timeout: 2000, interval: 5, describe: "falsy twice" }),
    "ok",
  );
  let threw = false;
  assert.equal(
    await waitFor(
      () => {
        if (!threw) {
          threw = true;
          throw new Error("boom");
        }
        return "ok";
      },
      { timeout: 2000, interval: 5, describe: "throws once" },
    ),
    "ok",
  );
  let err;
  try {
    await waitFor(() => 0, { timeout: 100, interval: 5, describe: "always falsy" });
  } catch (e) {
    err = e;
  }
  assert.ok(err, "an always-falsy poll must reject");
  assert.match(err.message, /timed out after 100ms waiting for always falsy/);
  assert.match(err.message, /last value: 0/);
  assert.doesNotMatch(err.message, /poll call did not return/);
});

test("cdp: send rejects when the browser never answers [F40-13]", async () => {
  const ws = fakeSocket();
  const cdp = new Cdp(ws, { commandTimeoutMs: 200 });
  const started = Date.now();
  const err = await rejection(cdp.send("Page.enable"), 700, "send did not reject within cap+500ms");
  assert.ok(Date.now() - started >= 150, "the command was rejected before its cap");
  assert.match(err.message, /Page\.enable/);
  // The id is gone: a late answer is ignored instead of throwing.
  const { id } = JSON.parse(ws.sent[0]);
  ws.emit("message", { data: JSON.stringify({ id, result: {} }) });
});

test("cdp: send rejects at once on a closed socket [F40-13]", async () => {
  const ws = fakeSocket({ readyState: 3 });
  const cdp = new Cdp(ws, { commandTimeoutMs: 200 });
  const err = await rejection(cdp.send("Browser.close"), 100, "send did not reject at once");
  assert.match(err.message, /Browser\.close/);
  assert.match(err.message, /not open/);
  assert.equal(ws.sent.length, 0, "a closed socket must not be written to");
});

test("cdp: fetchWithTimeout aborts a server that never answers [F40-14]", async () => {
  assert.equal(typeof harness.fetchWithTimeout, "function", "fetchWithTimeout did not abort: fetchWithTimeout is not exported");
  const silent = http.createServer(() => {});
  await new Promise((resolve) => silent.listen(0, "127.0.0.1", resolve));
  const answering = http.createServer((req, res) => res.end("fine"));
  await new Promise((resolve) => answering.listen(0, "127.0.0.1", resolve));
  try {
    const err = await rejection(
      harness.fetchWithTimeout(`http://127.0.0.1:${silent.address().port}/x`, {}, 200, "control /x"),
      700,
      "fetchWithTimeout did not abort",
    );
    assert.equal(err.message, "control /x: no answer within 200ms");
    const res = await harness.fetchWithTimeout(`http://127.0.0.1:${answering.address().port}/x`, {}, 2000, "control /x");
    assert.equal(await res.text(), "fine");
  } finally {
    // Connections first, and nobody awaits the close: a held connection must not keep this test alive.
    silent.closeAllConnections();
    silent.close();
    answering.closeAllConnections();
    answering.close();
  }
});

test("cdp: startChrome gives up when the DevTools socket never answers the upgrade [F40-13]", async () => {
  const sockets = [];
  const silent = net.createServer((socket) => {
    sockets.push(socket);
    socket.on("error", () => {});
  });
  await new Promise((resolve) => silent.listen(0, "127.0.0.1", resolve));
  const dir = mkdtempSync(join(tmpdir(), "liff-e2e-connect-"));
  const pidfile = join(dir, "pid");
  const chrome = join(dir, "fake-chrome");
  // A stand-in for Chrome: announces the port of the silent server and stays alive.
  writeFileSync(
    chrome,
    `#!${process.execPath}\n` +
      `require("node:fs").writeFileSync(${JSON.stringify(pidfile)}, String(process.pid));\n` +
      `process.stderr.write("DevTools listening on ws://127.0.0.1:${silent.address().port}/devtools/browser/fake\\n");\n` +
      `setInterval(() => {}, 1000);\n`,
  );
  chmodSync(chrome, 0o755);
  try {
    const started = harness._cleanupForTest.startChrome(chrome);
    // The guard below can win and the finally then destroys the socket: the late rejection has no waiter, and without
    // this catch node:test would record it as an unhandled rejection and turn the root red.
    started.catch(() => {});
    const err = await rejection(started, 6500, "startChrome did not give up on Cdp.connect within 6500ms");
    assert.match(err.message, /Cdp\.connect: no answer within/);
  } finally {
    for (const socket of sockets) socket.destroy();
    silent.close();
    try {
      const pid = Number(readFileSync(pidfile, "utf8"));
      for (const target of [-pid, pid]) {
        try {
          process.kill(target, "SIGKILL");
        } catch (e) {
          if (e.code !== "ESRCH" && e.code !== "EPERM") throw e;
        }
      }
    } catch (e) {
      if (e.code !== "ENOENT") throw e;
    }
    rmSync(dir, { recursive: true, force: true });
  }
});
