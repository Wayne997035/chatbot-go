// [F303-14] Responses that were already in flight when the account was deleted must not touch the page afterwards.
import assert from "node:assert/strict";
import { after, before, test } from "node:test";
import { waitFor } from "./lib/cdp.mjs";
import { startEnv } from "./lib/harness.mjs";

let env;

before(async () => {
  env = await startEnv();
});

after(async () => {
  await env?.stop();
});

const SEEDED_WEATHER = ["台北市", "新北市板橋區"];
const ERROR_BOXES = ["today-error", "trend-error", "records-error", "dv-error", "cal-error", "alerts-error", "account-error"];

async function withPage(body) {
  const page = await env.newPage();
  try {
    await body(page);
  } finally {
    await page.close();
  }
}

// Delete through the same buttons a user presses: settings tab, "delete my data", confirm.
async function deleteAccountViaUi(page) {
  await page.click('.tab[data-tab="settings"]');
  await page.click("#delete-open");
  await page.click("#delete-ok");
  await page.waitFor(`!document.getElementById("gate").hidden && !document.getElementById("gate-deleted").hidden`, {
    describe: "consent gate with the deleted notice",
  });
}

async function consentAgain(page) {
  await page.click("#consent-agree");
  await page.waitFor(`!document.getElementById("app").hidden && document.getElementById("status").hidden`, {
    describe: "app visible and the reload after consent finished",
  });
}

// What the alerts area shows right now.
function alertsView(page) {
  return page.eval(`(() => {
    const sw = (id) => document.getElementById(id);
    return {
      quake: sw("alert-quake-switch").getAttribute("aria-checked"),
      weather: sw("alert-weather-switch").getAttribute("aria-checked"),
      regions: sw("alert-weather-regions").textContent,
      regionsHidden: sw("alert-weather-regions").hidden,
      msg: sw("alerts-msg").textContent,
      msgHidden: sw("alerts-msg").hidden,
      quakeDisabled: sw("alert-quake-switch").disabled,
      weatherDisabled: sw("alert-weather-switch").disabled,
    };
  })()`);
}

// The held answer lands after the deletion and the new consent: the page must look exactly as the reload left it.
// Every check runs and all violations are reported together, so a red run says which of (a)-(d) broke.
async function releaseAndExpectUntouched(page, before, which) {
  await env.server.release();
  await waitHeldDrained();
  await page.settle();
  const now = await alertsView(page);
  const problems = [];
  const expect = (ok, text) => {
    if (!ok) problems.push(text);
  };
  expect(now.msg === "" && now.msgHidden, `(a) a stale response wrote into #alerts-msg: "${now.msg}" (hidden=${now.msgHidden})`);
  expect(now[`${which}Disabled`] === false, `(b) #alert-${which}-switch is still disabled after the held response arrived`);
  expect(now.quake === before.quake, `(c) #alert-quake-switch aria-checked changed from ${before.quake} to ${now.quake} after a stale response`);
  expect(now.weather === before.weather, `(c) #alert-weather-switch aria-checked changed from ${before.weather} to ${now.weather} after a stale response`);
  expect(
    now.regions === before.regions && now.regionsHidden === before.regionsHidden,
    `(d) #alert-weather-regions changed from "${before.regions}" to "${now.regions}" after a stale response`,
  );
  assert.deepEqual(problems, [], problems.join(" | "));
}

function waitHeldDrained() {
  return waitFor(async () => (await env.server.state()).held === 0, { describe: "no request held on the fake server" });
}

test("T07 stale loads and failures after account deletion are dropped [F303-14]", async () => {
  await withPage(async (page) => {
    await page.goto("/liff");
    // Four loads stay in flight: sync and summary would succeed, month and alerts would fail.
    await env.server.delay("/account/sync", "POST", { late: false });
    await env.server.delay("/summary", "GET", { late: false });
    await env.server.fail("/month", "GET", 500);
    await env.server.delay("/month", "GET", { late: false });
    await env.server.fail("/alerts", "GET", 500);
    await env.server.delay("/alerts", "GET", { late: false });
    await page.click("#consent-agree");
    await env.server.waitHeld(4);
    await env.server.clear(); // later loads (after the deletion) must be clean
    await deleteAccountViaUi(page);
    await env.server.release();
    await waitHeldDrained();
    await page.settle();

    assert.equal(await page.text("#account-name"), "", "a stale sync response wrote the member name");
    for (const id of ERROR_BOXES) {
      assert.equal(await page.text(`#${id}`), "", `a stale failure wrote an error into #${id}`);
    }
    assert.equal(await page.eval('document.getElementById("chart").childElementCount'), 0, "a stale summary response drew the chart");
    assert.equal(await page.text("#today-intake"), "0", "a stale summary response filled the today card");
    assert.equal(await page.hidden("#alerts-list"), false, "a stale alerts failure hid the switches");

    await consentAgain(page);
    const calls = (await env.server.state()).calls;
    const deletedAt = calls.findIndex((c) => c.method === "DELETE" && c.path === "/account");
    assert.ok(deletedAt >= 0, "the deletion call is missing from the call log");
    const firstAccountCall = calls.slice(deletedAt + 1).find((c) => c.path.startsWith("/account"));
    assert.equal(
      `${firstAccountCall?.method} ${firstAccountCall?.path}`,
      "POST /account/sync",
      "after consenting again the first account call must be the sync that stores the name again",
    );
  });
});

test("T07 stale profile saves (200 and 410) after account deletion are dropped [F303-14]", async () => {
  await withPage(async (page) => {
    await page.openApp();
    await page.click('.tab[data-tab="settings"]');
    // Two saves in flight: the first answer was computed before the deletion (200), the second after it (410).
    await env.server.delay("/profile", "PUT", { late: false });
    await env.server.delay("/profile", "PUT", { late: true });
    const submit = () =>
      page.eval(`(() => {
        const f = document.getElementById("profile-form");
        f.weightKg.value = "71"; f.heightCm.value = "175"; f.age.value = "30";
        f.querySelector('input[name="sex"][value="male"]').checked = true;
        f.requestSubmit();
      })()`);
    await submit();
    await submit();
    await env.server.waitHeld(2);
    await deleteAccountViaUi(page);
    await env.server.release();
    await waitHeldDrained();
    await page.settle();

    const status = await page.text("#status");
    assert.ok(!status.includes("已儲存"), `a stale save answered "saved" after the data was deleted: "${status}"`);
    assert.ok(!status.includes("讀取失敗"), `a stale 410 triggered a reload that reported a read failure: "${status}"`);
    assert.equal(await page.eval('document.getElementById("profile-form").weightKg.value'), "", "a stale save refilled the body data form");
    assert.equal(await page.text("#account-name"), "", "a stale reload wrote the member name");
  });
});

test("T08-quake-on a stale earthquake switch-on is dropped after account deletion [F303-14]", async () => {
  await withPage(async (page) => {
    await env.server.setAlerts(false, SEEDED_WEATHER);
    await page.openApp();
    await page.click('.tab[data-tab="settings"]');
    assert.equal((await alertsView(page)).quake, "false", "precondition: the earthquake switch starts off");
    await env.server.delay("/alerts/earthquake_tsunami", "PUT", { late: false });
    await page.click("#alert-quake-switch");
    await env.server.waitHeld(1);
    await deleteAccountViaUi(page);
    await consentAgain(page);
    const before = await alertsView(page);
    assert.equal(before.quake, "false", "precondition: after the deletion and consenting again the switch reads off");
    await releaseAndExpectUntouched(page, before, "quake");
  });
});

test("T08-weather-off a stale weather switch-off is dropped after account deletion [F303-14]", async () => {
  await withPage(async (page) => {
    await page.openApp();
    await page.click('.tab[data-tab="settings"]');
    assert.equal((await alertsView(page)).weather, "true", "precondition: the weather switch starts on");
    await env.server.delay("/alerts/weather_warning", "DELETE", { late: false });
    await page.click("#alert-weather-switch");
    await env.server.waitHeld(1);
    await deleteAccountViaUi(page);
    // The account is gone on the server; put the subscription back so the reload after consent reads it as on.
    await env.server.setAlerts(true, SEEDED_WEATHER);
    await consentAgain(page);
    const before = await alertsView(page);
    assert.equal(before.weather, "true", "precondition: after consenting again the weather switch reads on");
    await releaseAndExpectUntouched(page, before, "weather");
  });
});

test("T08-weather-regions a stale region submission is dropped after account deletion [F303-14]", async () => {
  await withPage(async (page) => {
    await env.server.setAlerts(true, null);
    await page.openApp();
    await page.click('.tab[data-tab="settings"]');
    assert.equal((await alertsView(page)).weather, "false", "precondition: weather alerts start off");
    await page.click("#alert-weather-switch");
    await page.waitFor(`!!document.querySelector("#picker-body .picker-county")`, { describe: "county list in the picker" });
    await page.eval(`[...document.querySelectorAll(".picker-county")].find((b) => b.textContent.startsWith("台北市")).click()`);
    await page.click('#picker-body input[type="checkbox"]'); // "whole county"
    await page.click("#picker-actions .primary"); // confirm the county
    await page.waitFor(`document.querySelector("#picker-actions .primary").textContent === "開啟通知"`, { describe: "submit button" });
    await env.server.delay("/alerts/weather_warning", "PUT", { late: false });
    await page.click("#picker-actions .primary"); // submit: the request is now in flight
    await env.server.waitHeld(1);
    // The sheet covers the page, so a real tap cannot reach "delete" now; the buttons are driven directly to model a
    // deletion that completes while the submission is still on the wire (for example from another device).
    await deleteAccountViaUi(page);
    await consentAgain(page);
    const before = await alertsView(page);
    assert.equal(before.weather, "false", "precondition: after consenting again weather alerts read off");
    await releaseAndExpectUntouched(page, before, "weather");
  });
});

// [F31-10] The failure branches carry the same generation check as the success branches. Each test first proves, with no
// deletion, that the failure text still shows (so "silent" cannot mean "never written"), then repeats it across a deletion.
test("T08-quake-on-fail a stale earthquake switch-on failure (500) is dropped after account deletion [F31-10]", async () => {
  await withPage(async (page) => {
    await env.server.setAlerts(false, SEEDED_WEATHER);
    await page.openApp();
    await page.click('.tab[data-tab="settings"]');
    await env.server.fail("/alerts/earthquake_tsunami", "PUT", 500);
    await page.click("#alert-quake-switch");
    await page.waitFor(`document.getElementById("alerts-msg").textContent === "開啟失敗，請稍後再試"`, {
      describe: "failure text shown when the account was not deleted",
    });
    assert.equal((await alertsView(page)).quakeDisabled, false, "control: the switch is unlocked after a failure");
    await env.server.delay("/alerts/earthquake_tsunami", "PUT", { late: false });
    await page.click("#alert-quake-switch");
    await env.server.waitHeld(1);
    await deleteAccountViaUi(page);
    await consentAgain(page);
    const before = await alertsView(page);
    assert.equal(before.quake, "false", "precondition: after the deletion and consenting again the switch reads off");
    await releaseAndExpectUntouched(page, before, "quake");
  });
});

test("T08-weather-off-fail a stale weather switch-off failure (410) is dropped after account deletion [F31-10]", async () => {
  await withPage(async (page) => {
    await page.openApp();
    await page.click('.tab[data-tab="settings"]');
    assert.equal((await alertsView(page)).weather, "true", "precondition: the weather switch starts on");
    await env.server.fail("/alerts/weather_warning", "DELETE", 410);
    await page.click("#alert-weather-switch");
    await page.waitFor(`document.getElementById("alerts-msg").textContent === "關閉失敗，請稍後再試"`, {
      describe: "failure text shown when the account was not deleted",
    });
    assert.equal((await alertsView(page)).weatherDisabled, false, "control: the switch is unlocked after a failure");
    await env.server.delay("/alerts/weather_warning", "DELETE", { late: false });
    await page.click("#alert-weather-switch");
    await env.server.waitHeld(1);
    await deleteAccountViaUi(page);
    await env.server.setAlerts(true, SEEDED_WEATHER);
    await consentAgain(page);
    const before = await alertsView(page);
    assert.equal(before.weather, "true", "precondition: after consenting again the weather switch reads on");
    await releaseAndExpectUntouched(page, before, "weather");
  });
});

// [F31-35] The single-record delete failure branch compares the generation too: a failure that answers after the account
// was deleted must not put "刪除失敗" on the fresh page. The control phase proves the text does show without a deletion.
const RECORD_DELETE_FAILED = "刪除失敗，請稍後再試";

test("T08-record-delete-fail a stale record-delete failure is dropped after account deletion [F31-35]", async () => {
  await withPage(async (page) => {
    await page.openApp();
    await page.eval("window.confirm = () => true");
    const deleteSelector = "#meal-list li button";
    await page.waitFor(`!!document.querySelector(${JSON.stringify(deleteSelector)})`, { describe: "a meal record with a delete button" });
    await env.server.fail("/meals/", "DELETE", 500);
    await page.click(deleteSelector);
    await page.waitFor(`document.getElementById("status").textContent === ${JSON.stringify(RECORD_DELETE_FAILED)}`, {
      describe: "record delete failure text shown when the account was not deleted",
    });
    assert.equal(await page.eval(`document.querySelector(${JSON.stringify(deleteSelector)}).disabled`), false, "control: the delete button is unlocked after a failure");

    await env.server.fail("/meals/", "DELETE", 500);
    await env.server.delay("/meals/", "DELETE", { late: true });
    await page.click(deleteSelector);
    await env.server.waitHeld(1);
    await deleteAccountViaUi(page);
    await consentAgain(page);
    assert.equal(await page.hidden("#status"), true, "precondition: the status line is hidden after the deletion and consenting again");
    const failedBefore = (await env.server.state()).calls.filter((c) => c.method === "DELETE" && c.path.startsWith("/meals/") && c.status === 500).length;

    await env.server.release();
    // The fake server records the status before it writes the answer, so waiting for the call is not enough: also wait
    // until nothing is held, then let the page process the answer.
    await waitFor(
      async () => (await env.server.state()).calls.filter((c) => c.method === "DELETE" && c.path.startsWith("/meals/") && c.status === 500).length > failedBefore,
      { describe: "the held DELETE /meals/ answered 500" },
    );
    await waitHeldDrained();
    await page.settle();
    const status = await page.eval(`({ text: document.getElementById("status").textContent, hidden: document.getElementById("status").hidden })`);
    assert.ok(!status.text.includes("刪除失敗"), `a stale record-delete failure wrote into #status: "${status.text}" (hidden=${status.hidden})`);
  });
});
