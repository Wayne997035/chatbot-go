// [F34-57] Direct editing of meals and workouts on the records tab: the form sends the version it was shown, the status line says
// what really happened for 200 / 409 / 400 / 404, and a 409 reloads the list and drops the draft.
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { after, before, test } from "node:test";
import { waitFor } from "./lib/cdp.mjs";
import { repoRoot, startEnv } from "./lib/harness.mjs";

let env;

before(async () => {
  env = await startEnv();
});

after(async () => {
  await env?.stop();
});

const DAY = JSON.parse(readFileSync(join(repoRoot, "internal/liff/testdata/day.json"), "utf8"));
const MEAL = DAY.meals[0];
const WORKOUT = DAY.workouts[0];

const STALE_RECORD_TEXT = "這筆剛在其他地方被修改，已重新載入，請再編輯一次";
const GONE_RECORD_TEXT = "這筆已經不存在，已重新載入";

async function withPage(body) {
  const page = await env.newPage();
  try {
    await body(page);
  } finally {
    await page.close();
  }
}

async function calls(method, pathPrefix) {
  return (await env.server.state()).calls.filter((c) => c.method === method && c.path.startsWith(pathPrefix));
}

// Open the edit form of the first row of a list the way a user does: press its "編輯" button.
async function openForm(page, listId) {
  await page.waitFor(`!!document.querySelector("#${listId} li .edit-toggle")`, { describe: `an edit button in #${listId}` });
  await page.click(`#${listId} li .edit-toggle`);
  await page.waitFor(`!!document.querySelector("#${listId} li .edit-form")`, { describe: `the edit form in #${listId}` });
}

// Type into the n-th input of the first meal item row; the input event runs the page's own macro scaling.
function setMealInput(page, index, value) {
  return page.eval(`(() => {
    const input = document.querySelector("#meal-list li .edit-form .edit-row").querySelectorAll("input")[${index}];
    input.value = ${JSON.stringify(String(value))};
    input.dispatchEvent(new Event("input", { bubbles: true }));
  })()`);
}

const MEAL_NAME = 0;
const MEAL_CALORIES = 2;

function submitForm(page, listId) {
  return page.click(`#${listId} li .edit-form button[type="submit"]`);
}

function statusText(page) {
  return page.eval(`document.getElementById("status").textContent`);
}

test("T16 meal edit form sends the viewed version and the edited item [F34-57]", async () => {
  await withPage(async (page) => {
    await page.openApp();
    const dayBefore = (await calls("GET", "/day")).length;
    await openForm(page, "meal-list");
    await setMealInput(page, MEAL_CALORIES, 480);
    await submitForm(page, "meal-list");
    await page.waitFor(`document.getElementById("status").textContent === "已儲存"`, { describe: "status 已儲存 after the PUT" });

    const puts = await calls("PUT", "/meals/");
    assert.equal(puts.length, 1, `expected one PUT /meals/, got ${JSON.stringify(puts)}`);
    assert.equal(puts[0].path, `/meals/${MEAL.id}`, "the PUT goes to the meal that was edited");
    const body = JSON.parse(puts[0].body);
    assert.deepEqual(Object.keys(body).sort(), ["items", "version"], `the body carries only version and items, got ${puts[0].body}`);
    assert.equal(body.version, MEAL.version, `the body must carry the version the page was shown (${MEAL.version}), got ${body.version}`);
    assert.equal(body.items.length, MEAL.items.length, "every item is sent");
    const first = MEAL.items[0];
    assert.equal(body.items[0].calories, 480, "the edited calories are sent");
    assert.deepEqual(Object.keys(body.items[0]).sort(), ["calories", "carbs_g", "fat_g", "name", "portion", "protein_g"]);
    const ratio = (g) => Math.round(((g * 480) / first.calories) * 10) / 10;
    assert.equal(body.items[0].protein_g, ratio(first.protein_g), "protein follows the calorie change");
    assert.equal(body.items[0].carbs_g, ratio(first.carbs_g), "carbs follow the calorie change");
    assert.equal(body.items[0].fat_g, ratio(first.fat_g), "fat follows the calorie change");
    assert.deepEqual(body.items[1], { name: MEAL.items[1].name, portion: MEAL.items[1].portion, calories: MEAL.items[1].calories, protein_g: MEAL.items[1].protein_g, carbs_g: MEAL.items[1].carbs_g, fat_g: MEAL.items[1].fat_g }, "an untouched item goes back as it was");
    await page.waitFor(`document.querySelector("#meal-list li .edit-form") === null`, { describe: "the form closed after the reload" });
    assert.equal((await calls("GET", "/day")).length, dayBefore + 1, "the list is reloaded once after the save");
  });
});

// Each failure answer must put its own sentence on the status line or in the form, and 409 / 404 must reload the list.
async function failedSave(status) {
  const result = {};
  await withPage(async (page) => {
    await page.openApp();
    const dayBefore = (await calls("GET", "/day")).length;
    await openForm(page, "meal-list");
    await setMealInput(page, MEAL_NAME, "改過的名字");
    await env.server.fail("/meals/", "PUT", status);
    await submitForm(page, "meal-list");
    await page.waitFor(
      `document.getElementById("status").textContent !== "" || (document.querySelector("#meal-list .edit-error") && !document.querySelector("#meal-list .edit-error").hidden)`,
      { describe: `a visible answer to the ${status}` },
    );
    await page.settle();
    Object.assign(result, await page.eval(`({
      status: document.getElementById("status").textContent,
      formOpen: document.querySelector("#meal-list li .edit-form") !== null,
      formError: document.querySelector("#meal-list .edit-error") ? document.querySelector("#meal-list .edit-error").textContent : "",
    })`));
    result.dayReloads = (await calls("GET", "/day")).length - dayBefore;
  });
  return result;
}

test("T17 a 409 on save says the record changed, reloads the list and drops the draft [F34-57]", async () => {
  const r = await failedSave(409);
  assert.equal(r.status, STALE_RECORD_TEXT, `409 status text, got "${r.status}"`);
  assert.equal(r.formOpen, false, "the form closes on a 409 (the draft is not kept)");
  assert.equal(r.dayReloads, 1, "a 409 reloads the list");
});

test("T17b a 400 on save keeps the form and shows the ranges in it [F34-57]", async () => {
  const r = await failedSave(400);
  assert.equal(r.formOpen, true, "the form stays open on a 400");
  assert.ok(r.formError.includes("熱量 0～3000"), `the form error must show the calorie range, got "${r.formError}"`);
  assert.ok(r.formError.includes("份量 30 字內"), `the form error must show the portion range, got "${r.formError}"`);
  assert.equal(r.dayReloads, 0, "a 400 does not reload");
});

test("T17c a 404 on save says the record is gone and reloads the list [F34-57]", async () => {
  const r = await failedSave(404);
  assert.equal(r.status, GONE_RECORD_TEXT, `404 status text, got "${r.status}"`);
  assert.equal(r.formOpen, false, "the form closes on a 404");
  assert.equal(r.dayReloads, 1, "a 404 reloads the list");
});

test("T19 workout edit form sends version and the four editable fields [F34-57]", async () => {
  await withPage(async (page) => {
    await page.openApp();
    await openForm(page, "workout-list");
    await page.eval(`(() => {
      const inputs = document.querySelector("#workout-list li .edit-form").querySelectorAll("input");
      inputs[0].value = "60"; // duration
      inputs[1].value = "7"; // distance
      inputs[2].value = "400"; // calories
    })()`);
    await submitForm(page, "workout-list");
    await page.waitFor(`document.getElementById("status").textContent === "已儲存"`, { describe: "status 已儲存 after the PUT" });

    const puts = await calls("PUT", "/workouts/");
    assert.equal(puts.length, 1, `expected one PUT /workouts/, got ${JSON.stringify(puts)}`);
    assert.equal(puts[0].path, `/workouts/${WORKOUT.id}`, "the PUT goes to the workout that was edited");
    const body = JSON.parse(puts[0].body);
    assert.deepEqual(Object.keys(body).sort(), ["activity", "calories", "distanceKm", "durationMin", "version"], `unexpected body ${puts[0].body}`);
    assert.equal(body.version, WORKOUT.version, `the body must carry the version the page was shown (${WORKOUT.version}), got ${body.version}`);
    assert.equal(body.activity, WORKOUT.activity, "the activity is sent as shown");
    assert.equal(body.durationMin, 60);
    assert.equal(body.distanceKm, 7);
    assert.equal(body.calories, 400);
    assert.equal(await statusText(page), "已儲存");
  });
});

// [F34-57] The remaining error branches of submitEdit, for both forms: the sentence the user reads is checked character for
// character (the expected text comes from the app's wording and the spec, not from running the page), and a 404 must really reload.
const SAVE_FAILED_RANGE_PREFIX = "儲存失敗，請確認欄位範圍：";
const SAVE_FAILED_LATER = "儲存失敗，請稍後再試";
const MEAL_RANGE_TEXT = "名稱 1～30 字、份量 30 字內、熱量 0～3000、蛋白質／碳水／脂肪 0～500、至少留一個品項、名稱與份量不可含網址";
const WORKOUT_RANGE_TEXT = "活動 1～20 字、時間 1～1440 分鐘、距離 0～500 公里、熱量 1～10000";

const FORMS = {
  meal: { list: "meal-list", prefix: "/meals/", range: MEAL_RANGE_TEXT, edit: (page) => setMealInput(page, MEAL_NAME, "改過的名字") },
  workout: {
    list: "workout-list",
    prefix: "/workouts/",
    range: WORKOUT_RANGE_TEXT,
    edit: (page) => page.eval(`(() => {
      const input = document.querySelector("#workout-list li .edit-form").querySelectorAll("input")[0];
      input.value = "61";
      input.dispatchEvent(new Event("input", { bubbles: true }));
    })()`),
  },
};

async function failedFormSave(kind, status) {
  const form = FORMS[kind];
  const result = {};
  await withPage(async (page) => {
    await page.openApp();
    const dayBefore = (await calls("GET", "/day")).length;
    await openForm(page, form.list);
    await form.edit(page);
    await env.server.fail(form.prefix, "PUT", status);
    await submitForm(page, form.list);
    await page.waitFor(
      `document.getElementById("status").textContent !== "" || (document.querySelector("#${form.list} .edit-error") && !document.querySelector("#${form.list} .edit-error").hidden)`,
      { describe: `a visible answer to the ${status}` },
    );
    await page.settle();
    Object.assign(result, await page.eval(`({
      status: document.getElementById("status").textContent,
      formOpen: document.querySelector("#${form.list} li .edit-form") !== null,
      formError: document.querySelector("#${form.list} .edit-error") ? document.querySelector("#${form.list} .edit-error").textContent : "",
      saveDisabled: document.querySelector("#${form.list} li .edit-form button[type=submit]")?.disabled ?? null,
    })`));
    result.dayReloads = (await calls("GET", "/day")).length - dayBefore;
  });
  return result;
}

for (const kind of ["meal", "workout"]) {
  test(`T17d ${kind} form: a 404 on save says the record is gone and reloads the list [F34-57]`, async () => {
    const r = await failedFormSave(kind, 404);
    assert.equal(r.status, GONE_RECORD_TEXT, `404 status text, got "${r.status}"`);
    assert.equal(r.formOpen, false, "the form closes on a 404");
    assert.equal(r.dayReloads, 1, "a 404 reloads the list exactly once");
  });

  test(`T17e ${kind} form: a 400 on save shows the range sentence inside the form and does not reload [F34-57]`, async () => {
    const r = await failedFormSave(kind, 400);
    assert.equal(r.formError, SAVE_FAILED_RANGE_PREFIX + FORMS[kind].range, `400 form error text, got "${r.formError}"`);
    assert.equal(r.formOpen, true, "the form stays open on a 400");
    assert.equal(r.saveDisabled, false, "the save button is usable again after a 400");
    assert.equal(r.dayReloads, 0, "a 400 does not reload");
  });

  test(`T17f ${kind} form: a 500 on save says to try again later inside the form and does not reload [F34-57]`, async () => {
    const r = await failedFormSave(kind, 500);
    assert.equal(r.formError, SAVE_FAILED_LATER, `500 form error text, got "${r.formError}"`);
    assert.equal(r.formOpen, true, "the form stays open on a 500");
    assert.equal(r.saveDisabled, false, "the save button is usable again after a 500");
    assert.equal(r.dayReloads, 0, "a 500 does not reload");
    assert.equal(r.status, "", "a failed save must not put 已儲存 or anything else on the status line");
  });
}

// [F34-57] The edit forms must fit the phone window: no sideways scroll, the cancel button inside the window (QA34-01).
for (const width of [320, 390]) {
  for (const kind of ["meal", "workout"]) {
    test(`T20 the ${kind} edit form fits a ${width}px wide window [F34-57]`, async () => {
      await withPage(async (page) => {
        await page.session.send("Emulation.setDeviceMetricsOverride", { width, height: 844, deviceScaleFactor: 1, mobile: false });
        await page.openApp();
        await page.click('.tab[data-tab="records"]'); // the lists sit in a hidden panel on the default tab: measure where the user sees them
        await openForm(page, FORMS[kind].list);
        const m = await page.eval(`(() => {
          const cancel = document.querySelector("#${FORMS[kind].list} li .edit-form .edit-cancel").getBoundingClientRect();
          return { scrollWidth: document.documentElement.scrollWidth, innerWidth: window.innerWidth, cancelRight: cancel.right };
        })()`);
        assert.ok(m.scrollWidth <= m.innerWidth, `${kind} form at ${width}px: scrollWidth ${m.scrollWidth} > innerWidth ${m.innerWidth}`);
        assert.ok(m.cancelRight <= m.innerWidth, `${kind} form at ${width}px: the cancel button's right edge ${m.cancelRight} is past innerWidth ${m.innerWidth}`);
      });
    });
  }
}

// [F34-57] The result of a save (已儲存 / 409 / 404) must be where the user can see it, also after scrolling down to the list:
// the status line is inside the window and holds the focus, so a screen reader reads it too (QA34-02 / FE34-R1-03).
for (const outcome of [
  { name: "saved", status: null, text: "已儲存" },
  { name: "409", status: 409, text: STALE_RECORD_TEXT },
  { name: "404", status: 404, text: GONE_RECORD_TEXT },
]) {
  test(`T21 a ${outcome.name} result on save is visible and focused after scrolling to the list [F34-57]`, async () => {
    await withPage(async (page) => {
      await page.session.send("Emulation.setDeviceMetricsOverride", { width: 390, height: 500, deviceScaleFactor: 1, mobile: false });
      await page.openApp();
      await page.click('.tab[data-tab="records"]');
      await openForm(page, "meal-list");
      await page.eval(`(() => {
        const pad = document.createElement("div");
        pad.style.height = "1500px"; // the page must be taller than the window so the header status can be out of view
        document.body.append(pad);
        document.querySelector("#meal-list li .edit-form").scrollIntoView({ block: "end" });
        window.scrollTo(0, document.documentElement.scrollHeight);
      })()`);
      assert.ok((await page.eval("window.scrollY")) > 300, "precondition: the page is scrolled down, away from the header");
      if (outcome.status) await env.server.fail("/meals/", "PUT", outcome.status);
      await submitForm(page, "meal-list");
      await page.waitFor(`document.getElementById("status").textContent === ${JSON.stringify(outcome.text)}`, { describe: `status ${outcome.text}` });
      await page.settle();
      const m = await page.eval(`(() => {
        const s = document.getElementById("status");
        const r = s.getBoundingClientRect();
        return { text: s.textContent, top: r.top, bottom: r.bottom, headerBottom: document.querySelector(".top").getBoundingClientRect().bottom, hit: (() => { const el = document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2); return !!el && (el === s || s.contains(el)); })(), innerHeight: window.innerHeight, active: document.activeElement && document.activeElement.id };
      })()`);
      assert.equal(m.text, outcome.text);
      assert.ok(m.top >= 0 && m.bottom <= m.innerHeight, `the ${outcome.name} status line is outside the window: top ${m.top}, bottom ${m.bottom}, innerHeight ${m.innerHeight}`);
      assert.ok(m.top >= m.headerBottom, `the ${outcome.name} status line: it sits under the sticky header (top ${m.top} < header bottom ${m.headerBottom})`);
      assert.ok(m.hit, `the ${outcome.name} status line: something else covers its centre point`);
      assert.equal(m.active, "status", `the focus must be on #status after a ${outcome.name} result, got "${m.active}"`);
    });
  });
}

// [F34-57] One form sends one PUT until its answer is back, however the submit event arrives (requestSubmit, Enter key); after a
// failure that leaves the form open the next save goes out again (QA34-03).
for (const kind of ["meal", "workout"]) {
  test(`T22 the ${kind} edit form sends one PUT while the first is in flight, and again after a 500 [F34-57]`, async () => {
    const form = FORMS[kind];
    await withPage(async (page) => {
      await page.openApp();
      await openForm(page, form.list);
      await form.edit(page);
      const submitThree = () => page.eval(`(() => {
        const f = document.querySelector("#${form.list} li .edit-form");
        f.requestSubmit(); f.requestSubmit(); f.requestSubmit();
      })()`);
      const puts = async () => (await calls("PUT", form.prefix)).length;

      await env.server.delay(form.prefix, "PUT", { ms: 2000 });
      await submitThree();
      await page.settle();
      assert.equal(await puts(), 1, `three submits while the first PUT is in flight must send one PUT, sent ${await puts()}`);
      await env.server.release();
      await page.waitFor(`document.getElementById("status").textContent === "已儲存"`, { describe: "status 已儲存 after the held PUT" });

      // 500 leaves the form open and must unlock it: a second save goes out.
      await openForm(page, form.list);
      await form.edit(page);
      await env.server.fail(form.prefix, "PUT", 500);
      const before = await puts();
      await submitThree();
      await page.waitFor(`document.querySelector("#${form.list} .edit-error") && !document.querySelector("#${form.list} .edit-error").hidden`, { describe: "the 500 error in the form" });
      assert.equal(await puts(), before + 1, "the failed save sent exactly one PUT");
      await env.server.clear();
      await page.eval(`document.querySelector("#${form.list} li .edit-form").requestSubmit()`);
      // the status line still reads 已儲存 from the first save, so wait for the second PUT itself
      await waitFor(async () => (await puts()) === before + 2, { describe: "the retry PUT after a 500" });
      assert.equal(await puts(), before + 2, "after a 500 the next save is sent again (the busy flag was reset)");
    });
  });
}

// [F34-57] Coming back to the page (foreground reload) must not throw away an open edit form and what was typed in it (FE34-R1-02);
// the rest of the page does reload, and a save still closes the form as before.
for (const kind of ["meal", "workout"]) {
  test(`T23 a foreground reload keeps the open ${kind} edit form and its typed value [F34-57]`, async () => {
    const form = FORMS[kind];
    await withPage(async (page) => {
      await page.openApp();
      await page.click('.tab[data-tab="records"]');
      await openForm(page, form.list);
      await form.edit(page);
      const typed = await page.eval(`document.querySelector("#${form.list} li .edit-form input").value`);
      const dayBefore = (await calls("GET", "/day")).length;
      // The harness freezes Date.now, which would hold the 5 s throttle shut: move the clock past it, then come back to the page.
      await page.eval(`(() => { const t = Date.now(); Date.now = () => t + 10000; })()`);
      await page.eval(`document.dispatchEvent(new Event("visibilitychange"))`); // visibilityState is "visible" in this browser
      await waitFor(async () => (await calls("GET", "/day")).length > dayBefore, { describe: "the foreground reload fetched /day again" });
      await page.settle();
      const after = await page.eval(`(() => {
        const input = document.querySelector("#${form.list} li .edit-form input");
        return { open: !!input, value: input ? input.value : null };
      })()`);
      assert.equal(after.open, true, "the foreground reload threw the open edit form away");
      assert.equal(after.value, typed, `the typed value must survive the reload, got "${after.value}" want "${typed}"`);
    });
  });
}

// [F34-57] The server refuses a URL in a meal item's name or portion, so the meal range sentence must say so; the workout form has
// no URL rule and its sentence stays as it was (FE34-R1-04).
test("T24 the meal 400 sentence names the URL rule, the workout one does not [F34-57]", async () => {
  const meal = await failedFormSave("meal", 400);
  assert.ok(meal.formError.includes("名稱與份量不可含網址"), `the meal form error must say names and portions cannot hold a URL, got "${meal.formError}"`);
  const workout = await failedFormSave("workout", 400);
  assert.ok(!workout.formError.includes("網址"), `the workout form has no URL rule, got "${workout.formError}"`);
});

// [F34-57] Keyboard and screen reader users: opening the form puts the focus on its first field, cancelling or collapsing it puts the
// focus back on that row's 編輯 button, and every field points at the form's error line with aria-describedby (FE34-R1-05).
for (const kind of ["meal", "workout"]) {
  const list = FORMS[kind].list;
  test(`T25 the ${kind} edit form moves the focus in and back out and links its fields to the error line [F34-57]`, async () => {
    await withPage(async (page) => {
      await page.openApp();
      await page.click('.tab[data-tab="records"]');
      await openForm(page, list);
      const opened = await page.eval(`(() => {
        const form = document.querySelector("#${list} li .edit-form");
        const first = form.querySelector("input, select");
        const err = form.querySelector(".edit-error");
        const fields = [...form.querySelectorAll("input, select")];
        const row = form.closest("li");
        const toggle = row.querySelector(".edit-toggle");
        return {
          formIsActive: document.activeElement === form,
          formLabel: form.getAttribute("aria-label"),
          toggleLabel: toggle ? toggle.getAttribute("aria-label") : null,
          active: document.activeElement ? document.activeElement.tagName : "none",
          errId: err.id,
          unlinked: fields.filter((f) => !err.id || f.getAttribute("aria-describedby") !== err.id).length,
          fields: fields.length,
        };
      })()`);
      // user decision 7ec908ae: the focus lands on the form itself (no keyboard pops up on a phone); the first Tab reaches the first field.
      assert.equal(opened.formIsActive, true, `after opening, the focus must be on the form itself, it is on <${opened.active}>`);
      // FE34-R3-01: the focused form names what is being edited, the same words as its row's edit button.
      assert.ok(opened.toggleLabel && opened.toggleLabel.startsWith("編輯 "), `the edit button label is "${opened.toggleLabel}"`);
      assert.equal(opened.formLabel, opened.toggleLabel, `the form's aria-label must name the record being edited, got "${opened.formLabel}"`);
      await page.key("Tab");
      const tabbed = await page.eval(`(() => {
        const form = document.querySelector("#${list} li .edit-form");
        return document.activeElement === form.querySelector("input, select");
      })()`);
      assert.equal(tabbed, true, "one Tab from the form must reach its first field");
      assert.ok(opened.errId, "the error line needs an id for aria-describedby");
      assert.equal(opened.unlinked, 0, `${opened.unlinked} of ${opened.fields} fields have no aria-describedby pointing at #${opened.errId}`);

      await page.click(`#${list} li .edit-form .edit-cancel`);
      const cancelled = await page.eval(`(() => ({
        formGone: document.querySelector("#${list} li .edit-form") === null,
        onToggle: document.activeElement === document.querySelector("#${list} li .edit-toggle"),
        active: document.activeElement ? document.activeElement.tagName + "." + document.activeElement.className : "none",
      }))()`);
      assert.equal(cancelled.formGone, true, "cancel closes the form");
      assert.equal(cancelled.onToggle, true, `after cancel the focus must be on the row's 編輯 button, it is on ${cancelled.active}`);

      // 收起 (the same button while the form is open) goes through the same close path.
      await openForm(page, list);
      await page.click(`#${list} li .edit-toggle`);
      const collapsed = await page.eval(`(() => ({
        formGone: document.querySelector("#${list} li .edit-form") === null,
        onToggle: document.activeElement === document.querySelector("#${list} li .edit-toggle"),
      }))()`);
      assert.equal(collapsed.formGone, true, "收起 closes the form");
      assert.equal(collapsed.onToggle, true, "after 收起 the focus stays on the 編輯 button");
    });
  });
}

// [F34-57] The same rule as T21 for the other actions fired from a record row: a failed delete and a refused scale change must show
// their message inside the window and hold the focus, also when the user scrolled down to the list (QA34-02, P3).
async function scrollToListAnd(page, act) {
  await page.session.send("Emulation.setDeviceMetricsOverride", { width: 390, height: 500, deviceScaleFactor: 1, mobile: false });
  await page.openApp();
  await page.click('.tab[data-tab="records"]');
  await page.eval(`(() => {
    window.confirm = () => true;
    const pad = document.createElement("div");
    pad.style.height = "1500px";
    document.body.append(pad);
    document.querySelector("#meal-list li").scrollIntoView({ block: "end" });
    window.scrollTo(0, document.documentElement.scrollHeight);
  })()`);
  assert.ok((await page.eval("window.scrollY")) > 300, "precondition: the page is scrolled down, away from the header");
  await act();
}

async function statusSeen(page, text) {
  await page.waitFor(`document.getElementById("status").textContent === ${JSON.stringify(text)}`, { describe: `status ${text}` });
  await page.settle();
  return page.eval(`(() => {
    const s = document.getElementById("status");
    const r = s.getBoundingClientRect();
    return { top: r.top, bottom: r.bottom, headerBottom: document.querySelector(".top").getBoundingClientRect().bottom, hit: (() => { const el = document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2); return !!el && (el === s || s.contains(el)); })(), innerHeight: window.innerHeight, active: document.activeElement && document.activeElement.id };
  })()`);
}

test("T26a a failed delete shows 刪除失敗 inside the window and focuses it after scrolling to the list [F34-57]", async () => {
  await withPage(async (page) => {
    await scrollToListAnd(page, async () => {
      await env.server.fail("/meals/", "DELETE", 500);
      await page.eval(`document.querySelector("#meal-list li button").click()`);
    });
    const m = await statusSeen(page, "刪除失敗，請稍後再試");
    assert.ok(m.top >= 0 && m.bottom <= m.innerHeight, `the delete failure is outside the window: top ${m.top}, bottom ${m.bottom}, innerHeight ${m.innerHeight}`);
      assert.ok(m.top >= m.headerBottom, `the delete failure: it sits under the sticky header (top ${m.top} < header bottom ${m.headerBottom})`);
      assert.ok(m.hit, `the delete failure: something else covers its centre point`);
    assert.equal(m.active, "status", `the focus must be on #status, got "${m.active}"`);
  });
});

test("T26b a 409 on a scale change shows its sentence inside the window and focuses it after scrolling to the list [F34-57]", async () => {
  await withPage(async (page) => {
    await scrollToListAnd(page, async () => {
      await env.server.fail("/meals/", "PATCH", 409);
      await page.eval(`(() => {
        const select = document.querySelector("#meal-list li .scale select");
        select.value = "1.5";
        select.dispatchEvent(new Event("change", { bubbles: true }));
      })()`);
    });
    const m = await statusSeen(page, "這筆剛在其他地方被修改，已重新載入，請再選一次份量");
    assert.ok(m.top >= 0 && m.bottom <= m.innerHeight, `the scale 409 sentence is outside the window: top ${m.top}, bottom ${m.bottom}, innerHeight ${m.innerHeight}`);
      assert.ok(m.top >= m.headerBottom, `the scale 409 sentence: it sits under the sticky header (top ${m.top} < header bottom ${m.headerBottom})`);
      assert.ok(m.hit, `the scale 409 sentence: something else covers its centre point`);
    assert.equal(m.active, "status", `the focus must be on #status, got "${m.active}"`);
  });
});

// [F34-57] A successful action from the list must not leave the previous action's message on the status line to be read as its result
// (QA34-R2-01): the list changing is the feedback, the status line is cleared.
async function dayLoads() {
  return (await calls("GET", "/day")).length;
}

test("T27a a successful delete clears the 已儲存 left by an earlier edit [F34-57]", async () => {
  await withPage(async (page) => {
    await page.openApp();
    await page.eval("window.confirm = () => true");
    await openForm(page, "meal-list");
    await setMealInput(page, MEAL_NAME, "改過的名字");
    await submitForm(page, "meal-list");
    await page.waitFor(`document.getElementById("status").textContent === "已儲存"`, { describe: "status 已儲存 after the edit" });
    await page.waitFor(`document.querySelectorAll("#meal-list li").length >= 2 && document.querySelector("#meal-list li .edit-form") === null`, { describe: "the list reloaded after the edit" });
    const before = await dayLoads();
    await page.eval(`document.querySelectorAll("#meal-list li")[1].querySelector("button").click()`); // the second row's delete button
    await waitFor(async () => (await dayLoads()) > before, { describe: "the list reloaded after the delete" });
    await page.settle();
    const s = await page.eval(`({ text: document.getElementById("status").textContent, hidden: document.getElementById("status").hidden })`);
    assert.ok(s.hidden && s.text === "", `a successful delete must clear the status line, it still says "${s.text}" (hidden=${s.hidden})`);
  });
});

test("T27b a successful scale change clears the 409 sentence left by an earlier try [F34-57]", async () => {
  await withPage(async (page) => {
    await page.openApp();
    const change = () => page.eval(`(() => {
      const select = document.querySelector("#meal-list li .scale select");
      select.value = "1.5";
      select.dispatchEvent(new Event("change", { bubbles: true }));
    })()`);
    await env.server.fail("/meals/", "PATCH", 409);
    await change();
    await page.waitFor(`document.getElementById("status").textContent === "這筆剛在其他地方被修改，已重新載入，請再選一次份量"`, { describe: "the 409 sentence" });
    await waitFor(async () => !(await page.eval(`document.querySelector("#meal-list li .scale select").disabled`)), { describe: "the list reloaded after the 409" });
    await env.server.clear();
    const before = await dayLoads();
    await change();
    await waitFor(async () => (await dayLoads()) > before, { describe: "the list reloaded after the scale change" });
    await page.settle();
    const s = await page.eval(`({ text: document.getElementById("status").textContent, hidden: document.getElementById("status").hidden })`);
    assert.ok(s.hidden && s.text === "", `a successful scale change must clear the status line, it still says "${s.text}" (hidden=${s.hidden})`);
  });
});
