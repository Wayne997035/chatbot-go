// [F303-12] Test harness: starts the Go fake server and a headless Chrome, hands every test a fresh isolated tab.
//
// The real page is served unchanged. Login is faked at the network layer: the tab pauses every request
// (Fetch domain), answers the LIFF SDK URL with a fake SDK, lets loopback requests through and fails everything else,
// so no test ever reaches the internet and the page bytes and CSP header stay the product's own.

import { spawn } from "node:child_process";
import { accessSync, constants, existsSync, mkdtempSync, readdirSync, readFileSync, rmSync } from "node:fs";
import { constants as osConstants, tmpdir } from "node:os";
import { delimiter, dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { Cdp, Session, sleep, waitFor } from "./cdp.mjs";

export const FAKE_ID_TOKEN = "e2e-fake-id-token";
export const SDK_URL = "https://static.line-scdn.net/liff/edge/2/sdk.js";
// app.js reads Date.now() once for "today"; freezing it pins the page to the month the golden files describe.
export const FROZEN_NOW = "2026-11-12T04:00:00Z";

const here = dirname(fileURLToPath(import.meta.url));
export const repoRoot = process.env.LIFF_E2E_REPO_ROOT || resolve(here, "../../../..");

// Only what app.js calls is defined: a missing method must turn a test red, not be swallowed.
const FAKE_SDK = `(function () {
  var calls = { init: 0, login: 0, closeWindow: 0, getIDToken: 0 };
  window.__e2eLiff = calls;
  window.liff = {
    init: function () { calls.init++; return Promise.resolve(); },
    isLoggedIn: function () { return true; },
    getIDToken: function () { calls.getIDToken++; return ${JSON.stringify(FAKE_ID_TOKEN)}; },
    login: function () { calls.login++; },
    closeWindow: function () { calls.closeWindow++; }
  };
})();`;

const FREEZE_CLOCK = `(function () {
  var fixed = Date.parse(${JSON.stringify(FROZEN_NOW)});
  Date.now = function () { return fixed; };
})();`;

// The Content-Security-Policy the page must be served with: the product's own constant, read from its source so the
// expectation moves with whichever version of the product is under test.
export function readCsp() {
  const lines = readFileSync(join(repoRoot, "internal/liff/page.go"), "utf8").split("\n");
  const start = lines.findIndex((l) => l.startsWith("const contentSecurityPolicy ="));
  if (start < 0) throw new Error("contentSecurityPolicy constant not found in internal/liff/page.go");
  let policy = "";
  for (let i = start; i < lines.length; i++) {
    for (const lit of lines[i].matchAll(/"((?:[^"\\]|\\.)*)"/g)) policy += JSON.parse(`"${lit[1]}"`);
    if (!lines[i].trimEnd().endsWith("+")) break;
  }
  if (!policy) throw new Error("contentSecurityPolicy constant is empty");
  return policy;
}

function executable(path) {
  try {
    accessSync(path, constants.X_OK);
    return true;
  } catch {
    return false;
  }
}

// LIFF_E2E_CHROME wins; otherwise google-chrome, chromium on PATH, then the macOS app bundle. Never a windowed fallback.
export function findChrome(env = process.env) {
  if (env.LIFF_E2E_CHROME) {
    if (!executable(env.LIFF_E2E_CHROME)) throw new Error(`LIFF_E2E_CHROME is not an executable file: ${env.LIFF_E2E_CHROME}`);
    return env.LIFF_E2E_CHROME;
  }
  for (const name of ["google-chrome", "chromium"]) {
    for (const dir of (env.PATH || "").split(delimiter).filter(Boolean)) {
      const candidate = join(dir, name);
      if (executable(candidate)) return candidate;
    }
  }
  const mac = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome";
  if (executable(mac)) return mac;
  throw new Error("Chrome not found. Install Google Chrome or set LIFF_E2E_CHROME to the browser executable.");
}

// Resolve once the child prints a line matching re on the given stream; reject if it exits first or time runs out.
function waitForLine(child, streamName, re, what, timeout = 20000) {
  return new Promise((resolvePromise, reject) => {
    let buf = "";
    const timer = setTimeout(() => done(new Error(`${what}: not ready after ${timeout}ms. Output so far:\n${buf}`)), timeout);
    const onData = (chunk) => {
      buf += chunk;
      const m = re.exec(buf);
      if (m) done(null, m);
    };
    const onExit = (code) => done(new Error(`${what} exited with code ${code} before it was ready. Output:\n${buf}`));
    function done(err, value) {
      clearTimeout(timer);
      child[streamName].off("data", onData);
      child.off("exit", onExit);
      if (err) reject(err);
      else resolvePromise({ match: value, output: () => buf });
    }
    child[streamName].setEncoding("utf8");
    child[streamName].on("data", onData);
    child.on("exit", onExit);
  });
}

async function stopChild(child, graceMs = 5000) {
  if (child.exitCode !== null || child.signalCode !== null) return;
  const exited = new Promise((r) => child.once("exit", r));
  child.kill("SIGTERM");
  const timer = setTimeout(() => child.kill("SIGKILL"), graceMs);
  await exited;
  clearTimeout(timer);
}

async function startServer() {
  const bin = process.env.LIFF_E2E_SERVER_BIN;
  if (!bin) throw new Error("LIFF_E2E_SERVER_BIN is not set; run the suite through `task e2e:liff` (it builds the fake server first).");
  const golden = join(repoRoot, "internal/liff/testdata");
  const child = spawn(bin, ["-addr", "127.0.0.1:0", "-golden-dir", golden], { stdio: ["ignore", "pipe", "pipe"] });
  let stderr = "";
  child.stderr.setEncoding("utf8");
  child.stderr.on("data", (c) => {
    stderr += c;
  });
  let ready;
  try {
    ready = await waitForLine(child, "stdout", /^LISTEN (\d+)$/m, "fake LIFF server");
  } catch (err) {
    await stopChild(child);
    throw new Error(`${err.message}\nserver stderr:\n${stderr}`);
  }
  const base = `http://127.0.0.1:${ready.match[1]}`;

  async function control(path, body) {
    const res = await fetch(`${base}/__e2e${path}`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    if (!res.ok) throw new Error(`control ${path} -> ${res.status}: ${await res.text()}`);
  }
  const server = {
    base,
    stderr: () => stderr,
    reset: () => control("/reset"),
    fail: (prefix, method, status) => control("/fail", { prefix, method, status }),
    // One-shot hold: the matching request waits (up to ms) until release(). late=false computes the answer first.
    delay: (prefix, method, { ms = 30000, late = false } = {}) => control("/delay", { prefix, method, ms, late }),
    clear: () => control("/clear"),
    release: () => control("/release"),
    setAlerts: (quake, weather) => control("/alerts", { quake, weather }),
    async state() {
      const res = await fetch(`${base}/__e2e/state`);
      return res.json();
    },
    // A direct call to the fake API as the page would make it (used to inspect or prepare server state).
    async call(method, path, body) {
      const res = await fetch(`${base}/api/v1/me${path}`, {
        method,
        headers: { authorization: `Bearer ${FAKE_ID_TOKEN}`, "content-type": "application/json" },
        body: body === undefined ? undefined : JSON.stringify(body),
      });
      const text = await res.text();
      return { status: res.status, text };
    },
    waitHeld: (n) => waitFor(async () => (await server.state()).held >= n, { describe: `${n} held request(s) on the fake server` }),
    stop: () => stopChild(child, 2000),
  };
  return server;
}

async function startChrome(executablePath) {
  const profile = mkdtempSync(join(tmpdir(), "liff-e2e-chrome-"));
  const args = [
    "--headless",
    "--remote-debugging-port=0",
    `--user-data-dir=${profile}`,
    "--no-first-run",
    "--disable-extensions",
  ];
  // Only for CI images where the sandbox cannot start; every request is confined to loopback by the Fetch handler.
  if (process.env.LIFF_E2E_NO_SANDBOX === "1") args.push("--no-sandbox");
  args.push("about:blank");
  // Chrome leads its own process group, so cleanup can end every helper process and not only the browser.
  const child = spawn(executablePath, args, { stdio: ["ignore", "ignore", "pipe"], detached: groups });
  liveGroups.add(child.pid);
  watchSignals();
  let ready;
  try {
    ready = await waitForLine(child, "stderr", /DevTools listening on (ws:\/\/\S+)/, "Chrome");
  } catch (err) {
    await stopChild(child);
    await endGroup(child.pid);
    removeProfile(profile);
    throw err;
  }
  const cdp = await Cdp.connect(ready.match[1]);
  const version = await cdp.send("Browser.getVersion");
  return {
    cdp,
    version: version.product,
    async stop() {
      try {
        await cdp.send("Browser.close");
      } catch {
        // The connection drops while the browser shuts down; the exit wait below is the real check.
      }
      await stopChild(child);
      await endGroup(child.pid);
      removeProfile(profile);
    },
  };
}

// Helper processes can outlive the browser process and keep writing into the profile, and on Node 22
// rmSync's retries do not delete files that appear meanwhile; so the whole group is killed and gone first.
const groups = process.platform !== "win32";
const liveGroups = new Set();

function signalGroup(pid, signal) {
  try {
    process.kill(-pid, signal);
    return true;
  } catch (err) {
    if (err.code === "ESRCH") return false;
    throw err;
  }
}

async function endGroup(pid, timeoutMs = 5000) {
  liveGroups.delete(pid);
  if (!groups) return;
  signalGroup(pid, "SIGKILL");
  const deadline = Date.now() + timeoutMs;
  while (signalGroup(pid, 0)) {
    if (Date.now() > deadline) throw new Error(`Chrome process group ${pid} is still alive ${timeoutMs}ms after SIGKILL`);
    await sleep(50);
  }
}

// A detached Chrome no longer gets the terminal's Ctrl-C, so the test process ends its groups on the way out.
let signalsWatched = false;
function watchSignals() {
  if (!groups || signalsWatched) return;
  signalsWatched = true;
  const killAll = () => {
    for (const pid of liveGroups) signalGroup(pid, "SIGKILL");
  };
  process.on("exit", killAll);
  for (const sig of ["SIGINT", "SIGTERM", "SIGHUP"]) {
    process.on(sig, () => {
      killAll();
      process.exit(128 + osConstants.signals[sig]);
    });
  }
}

function removeProfile(profile) {
  try {
    rmSync(profile, { recursive: true, force: true });
  } catch (err) {
    const left = existsSync(profile) ? readdirSync(profile, { recursive: true }).slice(0, 20).join(", ") : "";
    throw new Error(`could not remove the Chrome profile (${err.code}); still there: ${left}`, { cause: err });
  }
}

function isLoopback(url) {
  return ["127.0.0.1", "localhost", "[::1]"].includes(url.hostname);
}

// One isolated tab (own browser context, so localStorage never leaks between tests).
export class E2EPage {
  blockedRequests = [];
  requests = [];
  cspViolations = [];
  pageErrors = [];
  documentHeaders = null;
  #overrides = [];

  constructor(env, session, contextId, { fakeSdk }) {
    this.env = env;
    this.session = session;
    this.contextId = contextId;
    this.fakeSdk = fakeSdk;
  }

  async init() {
    const s = this.session;
    for (const domain of ["Page", "Runtime", "Network", "Log"]) await s.send(`${domain}.enable`);
    await s.send("Emulation.setFocusEmulationEnabled", { enabled: true });
    await s.send("Emulation.setDeviceMetricsOverride", { width: 390, height: 844, deviceScaleFactor: 1, mobile: false });
    await s.send("Page.addScriptToEvaluateOnNewDocument", { source: FREEZE_CLOCK });
    s.on("Network.requestWillBeSent", (p) => this.requests.push(p.request.url));
    s.on("Network.responseReceived", (p) => {
      if (p.type !== "Document") return;
      this.documentHeaders = Object.fromEntries(Object.entries(p.response.headers).map(([k, v]) => [k.toLowerCase(), v]));
    });
    s.on("Log.entryAdded", (p) => {
      const { source, level, text } = p.entry;
      if (source === "security" || /Content Security Policy/i.test(text)) this.cspViolations.push(text);
      else if (level === "error") this.pageErrors.push(text);
    });
    s.on("Runtime.exceptionThrown", (p) => this.pageErrors.push(p.exceptionDetails.exception?.description ?? p.exceptionDetails.text));
    s.on("Fetch.requestPaused", (p) => {
      this.#route(p).catch(() => {
        // The tab may already be gone when a late request is decided; nothing to answer then.
      });
    });
    await s.send("Fetch.enable", { patterns: [{ urlPattern: "*" }] });
  }

  async #route(p) {
    const s = this.session;
    const { requestId } = p;
    const url = new URL(p.request.url);
    if (p.request.url === SDK_URL && this.fakeSdk) {
      await s.send("Fetch.fulfillRequest", {
        requestId,
        responseCode: 200,
        responseHeaders: [
          { name: "Content-Type", value: "text/javascript" },
          { name: "Cache-Control", value: "no-store" },
        ],
        body: Buffer.from(FAKE_SDK).toString("base64"),
      });
      return;
    }
    const networked = url.protocol === "http:" || url.protocol === "https:";
    if (networked && !isLoopback(url)) {
      this.blockedRequests.push(p.request.url);
      await s.send("Fetch.failRequest", { requestId, errorReason: "BlockedByClient" });
      return;
    }
    const hit = networked ? this.#overrides.find((o) => o.match(url)) : undefined;
    if (hit) {
      await s.send("Fetch.fulfillRequest", {
        requestId,
        responseCode: 200,
        responseHeaders: [{ name: "Content-Type", value: "application/json" }],
        body: Buffer.from(JSON.stringify(hit.body)).toString("base64"),
      });
      return;
    }
    await s.send("Fetch.continueRequest", { requestId });
  }

  // Answer same-origin requests whose URL matches with a fixed JSON body instead of asking the fake server.
  override(match, body) {
    this.#overrides.push({ match, body });
  }

  async goto(path) {
    await this.session.send("Page.navigate", { url: `${this.env.server.base}${path}` });
    await this.waitFor(`document.readyState === "complete" && location.pathname === ${JSON.stringify(path)}`);
  }

  eval(expression, opts) {
    return this.session.eval(expression, opts);
  }

  waitFor(expression, opts) {
    return this.session.waitForExpr(expression, opts);
  }

  key(name, opts) {
    return this.session.key(name, opts);
  }

  async click(selector) {
    const sel = JSON.stringify(selector);
    await this.waitFor(`!!document.querySelector(${sel})`, { describe: `element ${selector}` });
    await this.eval(`document.querySelector(${sel}).click()`);
  }

  text(selector) {
    return this.eval(`document.querySelector(${JSON.stringify(selector)}).textContent`);
  }

  hidden(selector) {
    return this.eval(`document.querySelector(${JSON.stringify(selector)}).hidden`);
  }

  attr(selector, name) {
    return this.eval(`document.querySelector(${JSON.stringify(selector)}).getAttribute(${JSON.stringify(name)})`);
  }

  // Where focus is right now, described for failure messages: tag#id and whether it sits inside the given container.
  activeElement(containerSelector) {
    return this.eval(`(() => {
      const a = document.activeElement;
      const box = document.querySelector(${JSON.stringify(containerSelector)});
      const desc = a ? a.tagName.toLowerCase() + (a.id ? "#" + a.id : "") : "none";
      const text = a ? a.textContent.trim().slice(0, 24) : "";
      return { id: a ? a.id : "", text: text, desc: desc, inside: !!(box && a && box.contains(a)) };
    })()`);
  }

  // Let the page process responses that already arrived. Used only to prove a negative (nothing was written).
  settle() {
    return this.eval("new Promise((r) => setTimeout(r, 150))");
  }

  // Fresh consent flow: open the page, accept the notice, wait until the first load finished.
  async openApp() {
    await this.goto("/liff");
    await this.click("#consent-agree");
    await this.waitFor(`!document.getElementById("app").hidden && document.getElementById("status").hidden`, {
      describe: "app visible and first load finished",
    });
  }

  async close() {
    const cdp = this.env.chrome.cdp;
    await cdp.send("Target.disposeBrowserContext", { browserContextId: this.contextId }).catch(() => {
      // Already disposed with its last target.
    });
  }
}

// Start the fake server and Chrome; the caller stops them in an after() hook.
export async function startEnv() {
  const chromePath = findChrome();
  const server = await startServer();
  let chrome;
  try {
    chrome = await startChrome(chromePath);
  } catch (err) {
    await server.stop();
    throw err;
  }
  process.stderr.write(`# e2e browser: ${chrome.version} (${chromePath})\n`);
  const env = {
    server,
    chrome,
    chromePath,
    // fakeSdk=false leaves the SDK request to the loopback-only rule (it is blocked): the smoke test's own positive control.
    async newPage({ fakeSdk = true } = {}) {
      await server.reset();
      const { browserContextId } = await chrome.cdp.send("Target.createBrowserContext");
      const { targetId } = await chrome.cdp.send("Target.createTarget", { url: "about:blank", browserContextId });
      const { sessionId } = await chrome.cdp.send("Target.attachToTarget", { targetId, flatten: true });
      const page = new E2EPage(env, new Session(chrome.cdp, sessionId), browserContextId, { fakeSdk });
      await page.init();
      return page;
    },
    async stop() {
      // The server is stopped even when Chrome's cleanup fails: a live server keeps the test process from exiting.
      try {
        await chrome.stop();
      } finally {
        await server.stop();
      }
      await sleep(0);
    },
  };
  return env;
}
