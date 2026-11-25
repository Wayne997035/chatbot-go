// [F35-02] The records list names a snack by the Taiwan-time hour of its eatenAt: 14:00-17:59 reads "下午茶", every other hour "點心",
// whatever time zone the browser is in. The test runs the page in America/Los_Angeles so that a browser-local or UTC hour turns it red.
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { after, before, test } from "node:test";
import { repoRoot, startEnv } from "./lib/harness.mjs";

let env;

before(async () => {
  env = await startEnv();
});

after(async () => {
  await env?.stop();
});

const DAY = JSON.parse(readFileSync(join(repoRoot, "internal/liff/testdata/day.json"), "utf8"));

// One expected row per meal, in the order the page lists them. The window is written out here on purpose (14 and 18 are not read from app.js).
const ROWS = [
  { type: "snack", at: "2026-11-02T05:59:00Z", time: "13:59", label: "點心" },
  { type: "snack", at: "2026-11-02T06:00:00Z", time: "14:00", label: "下午茶" },
  { type: "snack", at: "2026-11-02T09:59:00Z", time: "17:59", label: "下午茶" },
  { type: "snack", at: "2026-11-02T10:00:00Z", time: "18:00", label: "點心" },
  { type: "snack", at: "2026-11-02T08:00:00Z", time: "16:00", label: "下午茶" },
  { type: "snack", at: "2026-11-02T15:00:00Z", time: "23:00", label: "點心" },
  { type: "lunch", at: "2026-11-02T06:00:00Z", time: "14:00", label: "午餐" },
];

function dayWithRows() {
  const base = DAY.meals[0];
  return {
    ...DAY,
    meals: ROWS.map((r, i) => ({ ...base, id: `tea-${i}`, eatenAt: r.at, mealType: r.type, place: undefined, source: "photo" })),
  };
}

test("T20 a snack reads 下午茶 only from 14:00 to 17:59 Taiwan time, in a browser set to America/Los_Angeles [F35-02]", async () => {
  const page = await env.newPage();
  try {
    await page.session.send("Emulation.setTimezoneOverride", { timezoneId: "America/Los_Angeles" });
    page.override((url) => url.pathname === "/api/v1/me/day", dayWithRows());
    await page.openApp();
    await page.waitFor(`document.querySelectorAll("#meal-list li.item").length === ${ROWS.length}`, { describe: "the seven meals listed" });

    const browserZone = await page.eval("Intl.DateTimeFormat().resolvedOptions().timeZone");
    assert.equal(browserZone, "America/Los_Angeles", "the browser time zone override did not take effect, the test would prove nothing");
    const metas = await page.eval(
      `Array.from(document.querySelectorAll("#meal-list li.item")).map((li) => li.querySelector(".item-meta").textContent)`,
    );
    assert.deepEqual(
      metas.map((m) => m.split("・").slice(0, 2).join("・")),
      ROWS.map((r) => `${r.time}・${r.label}`),
    );
  } finally {
    await page.close();
  }
});

// [F35-02] F35-FE-01: a snack whose eatenAt cannot be parsed reads "點心" instead of throwing, so the rest of the list still renders
// (before the guard, Intl.DateTimeFormat threw RangeError on the Invalid Date and the whole list stayed empty).
// The valid row sits at 11:00 Taiwan (03:00 UTC, 19:00 the evening before in Los Angeles): "點心" under every hour source,
// so this test only watches the guard and leaves the window and the hour source to T20.
test("T21 a snack with an unparseable eatenAt reads 點心 and the rest of the list still renders [F35-02]", async () => {
  const page = await env.newPage();
  try {
    const base = DAY.meals[0];
    const day = {
      ...DAY,
      meals: [
        { ...base, id: "tea-bad", eatenAt: "not-a-date", mealType: "snack", place: undefined, source: "photo" },
        { ...base, id: "tea-ok", eatenAt: "2026-11-02T03:00:00Z", mealType: "snack", place: undefined, source: "photo" },
      ],
    };
    page.override((url) => url.pathname === "/api/v1/me/day", day);
    await page.openApp();
    await page.waitFor(`document.querySelectorAll("#meal-list li.item").length === 2`, {
      describe: "both meals listed despite the unparseable eatenAt",
    });
    const labels = await page.eval(
      `Array.from(document.querySelectorAll("#meal-list li.item")).map((li) => li.querySelector(".item-meta").textContent.split("・")[1])`,
    );
    assert.deepEqual(labels, ["點心", "點心"]);
  } finally {
    await page.close();
  }
});

// [F36-09] An eatenAt that cannot be parsed shows "--:--" in the time slot instead of "Invalid Date" (same rows as T21; the workout list shares timeOf).
test("T22 a snack with an unparseable eatenAt shows --:-- instead of Invalid Date [F36-09]", async () => {
  const page = await env.newPage();
  try {
    const base = DAY.meals[0];
    const day = {
      ...DAY,
      meals: [
        { ...base, id: "time-bad", eatenAt: "not-a-date", mealType: "snack", place: undefined, source: "photo" },
        { ...base, id: "time-ok", eatenAt: "2026-11-02T03:00:00Z", mealType: "snack", place: undefined, source: "photo" },
      ],
    };
    page.override((url) => url.pathname === "/api/v1/me/day", day);
    await page.openApp();
    await page.waitFor(`document.querySelectorAll("#meal-list li.item").length === 2`, {
      describe: "both meals listed despite the unparseable eatenAt",
    });
    const metas = await page.eval(
      `Array.from(document.querySelectorAll("#meal-list li.item")).map((li) => li.querySelector(".item-meta").textContent)`,
    );
    assert.deepEqual(metas, ["--:--・點心", "11:00・點心"]);
    const listText = await page.eval(`document.getElementById("meal-list").textContent`);
    assert.ok(!listText.includes("Invalid Date"), `the meal list must not show Invalid Date, got: ${listText}`);
  } finally {
    await page.close();
  }
});

// [F36-10] The meta line reads time・label once; 打字估算 is appended only for typed meals. Expected sentences are written out (Taiwan 15:00 is tea time).
test("T23 the meta line reads time・label once, with 打字估算 only for typed meals [F36-10]", async () => {
  const page = await env.newPage();
  try {
    const base = DAY.meals[0];
    const day = {
      ...DAY,
      meals: [
        { ...base, id: "meta-photo", eatenAt: "2026-11-02T07:00:00Z", mealType: "snack", place: undefined, source: "photo" },
        { ...base, id: "meta-text", eatenAt: "2026-11-02T03:00:00Z", mealType: "snack", place: undefined, source: "text" },
      ],
    };
    page.override((url) => url.pathname === "/api/v1/me/day", day);
    await page.openApp();
    await page.waitFor(`document.querySelectorAll("#meal-list li.item").length === 2`, { describe: "both meals listed" });
    const metas = await page.eval(
      `Array.from(document.querySelectorAll("#meal-list li.item")).map((li) => li.querySelector(".item-meta").textContent)`,
    );
    assert.deepEqual(metas, ["15:00・下午茶", "11:00・點心・打字估算"]);
  } finally {
    await page.close();
  }
});

// [F36-10] The Taipei-hour formatter is a module-level singleton: building it once at page load, however many snack rows render.
// An Intl.DateTimeFormat constructor counter is injected before the page script runs; the old code built one formatter per taipeiHour call.
const COUNT_DTF = `(() => {
  const Orig = Intl.DateTimeFormat;
  window.__dtfCount = 0;
  function Counted(...args) {
    window.__dtfCount += 1;
    return new Orig(...args);
  }
  Counted.prototype = Orig.prototype;
  Counted.supportedLocalesOf = Orig.supportedLocalesOf.bind(Orig);
  Intl.DateTimeFormat = Counted;
})();`;

async function dtfCountFor(rowCount) {
  const page = await env.newPage();
  try {
    await page.session.send("Page.addScriptToEvaluateOnNewDocument", { source: COUNT_DTF });
    const base = DAY.meals[0];
    // Half of the rows inside 14:00-17:59 Taiwan time, half outside, so both branches of mealLabel run.
    const meals = Array.from({ length: rowCount }, (_, i) => ({
      ...base,
      id: `fmt-${i}`,
      eatenAt: i % 2 === 0 ? "2026-11-02T07:00:00Z" : "2026-11-02T03:00:00Z",
      mealType: "snack",
      place: undefined,
      source: "photo",
    }));
    page.override((url) => url.pathname === "/api/v1/me/day", { ...DAY, meals });
    await page.openApp();
    await page.waitFor(`document.querySelectorAll("#meal-list li.item").length === ${rowCount}`, { describe: `${rowCount} meals listed` });
    return await page.eval("window.__dtfCount");
  } finally {
    await page.close();
  }
}

test("T24 the Taipei-hour formatter is built once however many snack rows render [F36-10]", async () => {
  const few = await dtfCountFor(3);
  const many = await dtfCountFor(6);
  assert.equal(many, few, `Intl.DateTimeFormat was built ${few} times for 3 rows and ${many} times for 6 rows: the count must not grow with the rows`);
  assert.ok(many <= 1, `Intl.DateTimeFormat was built ${many} times, expected at most once per page`);
});

// [F37-11] A browser without the Asia/Taipei zone: Intl.DateTimeFormat (taipeiHour) and Date.prototype.toLocaleTimeString (timeOf) both
// throw RangeError for it. The two injections are independent; the page must still open and list the rows.
const NO_TAIPEI_DTF = `(() => {
  const Orig = Intl.DateTimeFormat;
  window.__taipeiDtfAttempts = 0;
  function Broken(...args) {
    if (args[1] && args[1].timeZone === "Asia/Taipei") {
      window.__taipeiDtfAttempts += 1;
      throw new RangeError("Invalid time zone specified: Asia/Taipei");
    }
    return new Orig(...args);
  }
  Broken.prototype = Orig.prototype;
  Broken.supportedLocalesOf = Orig.supportedLocalesOf.bind(Orig);
  Intl.DateTimeFormat = Broken;
})();`;
const NO_TAIPEI_TIME = `(() => {
  const orig = Date.prototype.toLocaleTimeString;
  Date.prototype.toLocaleTimeString = function (locale, options) {
    if (options && options.timeZone === "Asia/Taipei") throw new RangeError("Invalid time zone specified: Asia/Taipei");
    return orig.call(this, locale, options);
  };
})();`;

async function metasWithoutTaipei(injections) {
  const page = await env.newPage();
  try {
    for (const source of injections) await page.session.send("Page.addScriptToEvaluateOnNewDocument", { source });
    const base = DAY.meals[0];
    const meals = [
      { ...base, id: "nz-snack-1", eatenAt: "2026-11-02T07:00:00Z", mealType: "snack", place: undefined, source: "photo" },
      { ...base, id: "nz-lunch", eatenAt: "2026-11-02T04:00:00Z", mealType: "lunch", place: undefined, source: "photo" },
      { ...base, id: "nz-snack-2", eatenAt: "2026-11-02T08:00:00Z", mealType: "snack", place: undefined, source: "photo" },
    ];
    page.override((url) => url.pathname === "/api/v1/me/day", { ...DAY, meals });
    await page.openApp();
    await page.waitFor(`document.querySelectorAll("#meal-list li.item").length === 3`, { describe: "the three meals listed although Asia/Taipei is unsupported" });
    const metas = await page.eval(
      `Array.from(document.querySelectorAll("#meal-list li.item")).map((li) => li.querySelector(".item-meta").textContent)`,
    );
    const attempts = await page.eval("window.__taipeiDtfAttempts");
    return { metas, attempts, pageErrors: [...page.pageErrors] };
  } finally {
    await page.close();
  }
}

test("T28 the page opens and lists rows when the browser has no Asia/Taipei zone [F37-11]", async () => {
  // (a) only taipeiHour fails: snacks read 點心, times are the real ones.
  const a = await metasWithoutTaipei([NO_TAIPEI_DTF]);
  assert.deepEqual(a.pageErrors, [], "the page raised an exception when Intl.DateTimeFormat rejects Asia/Taipei");
  assert.deepEqual(a.metas.map((m) => m.split("・").slice(0, 2).join("・")), ["15:00・點心", "12:00・午餐", "16:00・點心"]);
  assert.ok(a.attempts <= 1, `the Taipei formatter was attempted ${a.attempts} times, a failure must not be retried per row`);
  // (a)+(b) both fail: time reads --:--, the rows are still there, and non-snack labels are untouched.
  const ab = await metasWithoutTaipei([NO_TAIPEI_DTF, NO_TAIPEI_TIME]);
  assert.deepEqual(ab.pageErrors, [], "the page raised an exception when Asia/Taipei is unsupported");
  assert.deepEqual(ab.metas.map((m) => m.split("・").slice(0, 2).join("・")), ["--:--・點心", "--:--・午餐", "--:--・點心"]);
  assert.ok(ab.attempts <= 1, `the Taipei formatter was attempted ${ab.attempts} times, a failure must not be retried per row`);
});
