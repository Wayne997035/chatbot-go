// [F303-12] Zero-dependency Chrome DevTools Protocol client.
// Uses only the WebSocket and fetch globals that ship with Node 22+. Protocol reference:
// https://chromedevtools.github.io/devtools-protocol/

export function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

// Poll fn until it returns something truthy. Never a fixed sleep: on timeout the last value is printed so a red run says why.
export async function waitFor(fn, { timeout = 5000, interval = 25, describe = "condition" } = {}) {
  const deadline = Date.now() + timeout;
  let last;
  for (;;) {
    try {
      last = await fn();
      if (last) return last;
    } catch (err) {
      last = `threw: ${err.message}`;
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

  static connect(url) {
    return new Promise((resolve, reject) => {
      const ws = new WebSocket(url);
      const cdp = new Cdp(ws);
      ws.addEventListener("open", () => resolve(cdp), { once: true });
      ws.addEventListener("error", () => reject(new Error(`cannot connect to ${url}`)), { once: true });
    });
  }

  constructor(ws) {
    this.#ws = ws;
    ws.addEventListener("message", (ev) => this.#onMessage(ev.data));
    ws.addEventListener("close", () => {
      for (const { reject } of this.#pending.values()) reject(new Error("CDP connection closed"));
      this.#pending.clear();
    });
  }

  #onMessage(data) {
    const msg = JSON.parse(String(data));
    if (msg.id !== undefined) {
      const slot = this.#pending.get(msg.id);
      if (!slot) return;
      this.#pending.delete(msg.id);
      if (msg.error) slot.reject(new Error(`${slot.method}: ${msg.error.message}`));
      else slot.resolve(msg.result ?? {});
      return;
    }
    for (const listener of this.#listeners) listener(msg);
  }

  send(method, params = {}, sessionId) {
    const id = this.#nextId++;
    const payload = { id, method, params };
    if (sessionId) payload.sessionId = sessionId;
    return new Promise((resolve, reject) => {
      this.#pending.set(id, { resolve, reject, method });
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
