// [F303-12] Zero-dependency Chrome DevTools Protocol client.
// Uses only the WebSocket and fetch globals that ship with Node 22+. Protocol reference:
// https://chromedevtools.github.io/devtools-protocol/

export function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

// Reject if promise has not settled within ms. The late result of the original promise is swallowed on purpose:
// by then the caller has already failed and cleaned up, and an unhandled rejection would crash the test process.
export function withTimeout(promise, ms, what) {
  promise.catch(() => {
    // Reported through the race below when it is first; afterwards nobody is waiting for it.
  });
  let timer;
  const timeout = new Promise((_, reject) => {
    timer = setTimeout(() => reject(new Error(`${what}: no answer within ${ms}ms`)), ms);
  });
  return Promise.race([promise, timeout]).finally(() => clearTimeout(timer));
}

const POLL_HUNG = Symbol("poll did not return");

// Poll fn until it returns something truthy. Never a fixed sleep: on timeout the last value is printed so a red run says why.
// [F40-12] Every poll is raced against the time left, so one poll that never returns (a browser that stopped answering)
// ends the wait at the deadline instead of holding the test until the runner's timeout.
export async function waitFor(fn, { timeout = 5000, interval = 25, describe = "condition" } = {}) {
  const deadline = Date.now() + timeout;
  let last;
  for (;;) {
    let timer;
    let hungOut = false;
    const hung = new Promise((resolve) => {
      timer = setTimeout(resolve, Math.max(deadline - Date.now(), 1), POLL_HUNG);
    });
    try {
      const pending = Promise.resolve().then(fn);
      // A poll that settles after the race is lost must not surface as an unhandled rejection.
      pending.catch(() => {});
      const got = await Promise.race([pending, hung]);
      if (got === POLL_HUNG) {
        hungOut = true;
      } else {
        last = got;
        if (last) return last;
      }
    } catch (err) {
      last = `threw: ${err.message}`;
    } finally {
      clearTimeout(timer);
    }
    if (hungOut) {
      throw new Error(`timed out after ${timeout}ms waiting for ${describe}; poll call did not return; last value: ${JSON.stringify(last)}`);
    }
    if (Date.now() > deadline) {
      throw new Error(`timed out after ${timeout}ms waiting for ${describe}; last value: ${JSON.stringify(last)}`);
    }
    await sleep(interval);
  }
}

export class Cdp {
  #ws;
  #nextId = 1;
  #pending = new Map();
  #listeners = new Set();
  #commandTimeoutMs;

  static connect(url, opts) {
    return new Promise((resolve, reject) => {
      const ws = new WebSocket(url);
      const cdp = new Cdp(ws, opts);
      ws.addEventListener("open", () => resolve(cdp), { once: true });
      ws.addEventListener("error", () => reject(new Error(`cannot connect to ${url}`)), { once: true });
    });
  }

  // [F40-13] commandTimeoutMs bounds every command that is not given its own limit.
  constructor(ws, { commandTimeoutMs = 15000 } = {}) {
    this.#ws = ws;
    this.#commandTimeoutMs = commandTimeoutMs;
    ws.addEventListener("message", (ev) => this.#onMessage(ev.data));
    ws.addEventListener("close", () => {
      for (const { reject, timer } of this.#pending.values()) {
        clearTimeout(timer);
        reject(new Error("CDP connection closed"));
      }
      this.#pending.clear();
    });
  }

  #onMessage(data) {
    const msg = JSON.parse(String(data));
    if (msg.id !== undefined) {
      const slot = this.#pending.get(msg.id);
      if (!slot) return;
      this.#pending.delete(msg.id);
      clearTimeout(slot.timer);
      if (msg.error) slot.reject(new Error(`${slot.method}: ${msg.error.message}`));
      else slot.resolve(msg.result ?? {});
      return;
    }
    for (const listener of this.#listeners) listener(msg);
  }

  // [F40-13] A socket that is not open drops send() without an error, and a browser can stay silent: so a closed socket
  // fails at once and every command has a limit, both naming the method. The timer is not unref'd: an unanswered
  // command must keep the event loop alive long enough to fail itself.
  send(method, params = {}, sessionId, { timeoutMs = this.#commandTimeoutMs } = {}) {
    if (this.#ws.readyState !== 1) {
      return Promise.reject(new Error(`CDP ${method}: connection is not open (readyState ${this.#ws.readyState})`));
    }
    const id = this.#nextId++;
    const payload = { id, method, params };
    if (sessionId) payload.sessionId = sessionId;
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => {
        this.#pending.delete(id);
        reject(new Error(`CDP ${method}: no answer within ${timeoutMs}ms`));
      }, timeoutMs);
      this.#pending.set(id, { resolve, reject, method, timer });
      this.#ws.send(JSON.stringify(payload));
    });
  }

  // listener receives { method, params, sessionId }; returns an unsubscribe function.
  onEvent(listener) {
    this.#listeners.add(listener);
    return () => this.#listeners.delete(listener);
  }

  close() {
    this.#ws.close();
  }
}

const KEYS = {
  Tab: { code: "Tab", keyCode: 9 },
  Escape: { code: "Escape", keyCode: 27 },
  Enter: { code: "Enter", keyCode: 13 },
};

// One attached target (a tab). Commands carry the session id; events are filtered to it.
export class Session {
  constructor(cdp, sessionId) {
    this.cdp = cdp;
    this.sessionId = sessionId;
  }

  send(method, params = {}) {
    return this.cdp.send(method, params, this.sessionId);
  }

  on(method, handler) {
    return this.cdp.onEvent((msg) => {
      if (msg.sessionId === this.sessionId && msg.method === method) handler(msg.params);
    });
  }

  // Evaluate an expression in the page and return its value; page exceptions become Errors.
  async eval(expression, { awaitPromise = true } = {}) {
    const res = await this.send("Runtime.evaluate", { expression, returnByValue: true, awaitPromise });
    if (res.exceptionDetails) {
      const d = res.exceptionDetails;
      throw new Error(`page exception: ${d.exception?.description ?? d.text}`);
    }
    return res.result.value;
  }

  // Poll a page expression until it is truthy.
  waitForExpr(expression, opts = {}) {
    return waitFor(() => this.eval(expression), { describe: expression, ...opts });
  }

  // A real key press through Input.dispatchKeyEvent (default actions such as Tab focus movement run).
  async key(name, { shift = false } = {}) {
    const info = KEYS[name];
    if (!info) throw new Error(`unknown key ${name}`);
    const base = { key: name, code: info.code, windowsVirtualKeyCode: info.keyCode, modifiers: shift ? 8 : 0 };
    await this.send("Input.dispatchKeyEvent", { type: "rawKeyDown", ...base });
    await this.send("Input.dispatchKeyEvent", { type: "keyUp", ...base });
  }
}
