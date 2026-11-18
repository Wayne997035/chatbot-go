// [F34-57] Direct editing of meals and workouts on the records tab: the form sends the version it was shown, the status line says
// what really happened for 200 / 409 / 400 / 404, and a 409 reloads the list and drops the draft.
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
