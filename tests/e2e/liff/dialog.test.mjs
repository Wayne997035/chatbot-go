// [F303-13] Dialog behaviour of the delete-account sheet and the region picker, driven through real key events.
import assert from "node:assert/strict";
import { after, before, describe, test } from "node:test";
import { startEnv } from "./lib/harness.mjs";

let env;

before(async () => {
  env = await startEnv();
});

after(async () => {
  await env?.stop();
});

const DELETE_BOX = "#delete-confirm";
const PICKER_BOX = "#picker";
const BACKGROUND = ['document.querySelector("#app > header.top")', 'document.querySelector("#app > main")'];

// Run body with a fresh tab and always dispose it.
async function withPage(body) {
  const page = await env.newPage();
  try {
    await body(page);
  } finally {
    await page.close();
  }
}

async function openDeleteDialog(page) {
  await page.openApp();
  await page.click('.tab[data-tab="settings"]');
  await page.click("#delete-open");
  await page.waitFor(`!document.querySelector("${DELETE_BOX}").hidden`, { describe: "delete dialog open" });
}

async function openPicker(page) {
  await page.openApp();
  await page.click('.tab[data-tab="settings"]');
  await page.click("#alert-weather-edit");
  await page.waitFor(`!document.querySelector("${PICKER_BOX}").hidden && !!document.querySelector("#picker-body .picker-county")`, {
    describe: "region picker open with the county list loaded",
  });
}

async function backgroundInert(page) {
  return page.eval(`[${BACKGROUND.map((n) => `${n}.inert`).join(", ")}]`);
}

// Press a key n times; after each press the focus must still be inside the box and be the expected element.
async function pressAndExpect(page, box, key, shift, expected, label) {
  for (let i = 0; i < expected.length; i++) {
    await page.key(key, { shift });
    const at = await page.activeElement(box);
    assert.ok(at.inside, `${label} #${i + 1}: focus left the dialog (activeElement=${at.desc})`);
    assert.equal(at.id, expected[i], `${label} #${i + 1}: focus moved to the wrong element in the dialog`);
  }
}

test("T01 Tab and Shift+Tab cycle inside the delete dialog [F303-13]", async () => {
  await withPage(async (page) => {
    await openDeleteDialog(page);
    const start = await page.activeElement(DELETE_BOX);
    assert.equal(start.id, "delete-cancel", `the safe button must hold focus on open (activeElement=${start.desc})`);
    // Two focusable buttons: cancel (first) and confirm (last). Tab from the last wraps to the first.
    const forward = ["delete-ok", "delete-cancel", "delete-ok", "delete-cancel", "delete-ok", "delete-cancel", "delete-ok"];
    await pressAndExpect(page, DELETE_BOX, "Tab", false, forward, "Tab");
    // Shift+Tab from the first wraps to the last.
    const backward = ["delete-cancel", "delete-ok", "delete-cancel", "delete-ok", "delete-cancel", "delete-ok", "delete-cancel"];
    await pressAndExpect(page, DELETE_BOX, "Tab", true, backward, "Shift+Tab");
  });
});

test("T02 Escape closes the delete dialog and focus returns to the opener [F303-13]", async () => {
  await withPage(async (page) => {
    await openDeleteDialog(page);
    await page.key("Escape");
    const hidden = await page.hidden(DELETE_BOX);
    assert.equal(hidden, true, "Escape did not close the delete dialog (hidden=false)");
    const at = await page.activeElement("body");
    assert.equal(at.id, "delete-open", `focus must return to the button that opened the dialog (activeElement=${at.desc})`);
  });
});

test("T03 the page behind an open dialog is inert and wakes up on close [F303-13]", async () => {
  await withPage(async (page) => {
    await openDeleteDialog(page);
    assert.deepEqual(await backgroundInert(page), [true, true], "header and main must be inert while the dialog is open");
    await page.click("#delete-cancel");
    assert.equal(await page.hidden(DELETE_BOX), true, "cancel must close the dialog when no deletion is running");
    assert.deepEqual(await backgroundInert(page), [false, false], "header and main must be interactive again after the dialog closed");
  });
});

test("T04 the region picker traps focus, closes on Escape and gives focus back [F303-13]", async () => {
  await withPage(async (page) => {
    await openPicker(page);
    const start = await page.activeElement(PICKER_BOX);
    assert.ok(start.inside, `focus must move into the picker when it opens (activeElement=${start.desc})`);
    assert.deepEqual(await backgroundInert(page), [true, true], "header and main must be inert while the picker is open");
    // Shift+Tab from the first control wraps to the last one (the last action button); Tab from there wraps back.
    await page.key("Tab", { shift: true });
    const last = await page.activeElement(PICKER_BOX);
    assert.ok(last.inside, `Shift+Tab: focus left the dialog (activeElement=${last.desc})`);
    const lastLabel = await page.text("#picker-actions > :last-child");
    assert.equal(last.text, lastLabel, "Shift+Tab from the first control must land on the last one (the last action button)");
    await page.key("Tab");
    const first = await page.activeElement(PICKER_BOX);
    assert.ok(first.inside, `Tab: focus left the dialog (activeElement=${first.desc})`);
    assert.equal(first.id === "" && first.text.startsWith("台北市"), true, `Tab from the last control must land on the first county, got "${first.text}"`);
    await page.key("Escape");
    assert.equal(await page.hidden(PICKER_BOX), true, "Escape did not close the region picker (hidden=false)");
    const back = await page.activeElement("body");
    assert.equal(back.id, "alert-weather-edit", `focus must return to the control that opened the picker (activeElement=${back.desc})`);
    assert.deepEqual(await backgroundInert(page), [false, false], "header and main must be interactive again after the picker closed");
  });
});

// While DELETE /account is in flight the dialog is locked: neither cancel nor Escape may close it.
describe("T09 / T09b deletion in flight locks the dialog [F303-13]", () => {
  async function startDeletionAndHold(page) {
    await openDeleteDialog(page);
    await env.server.delay("/account", "DELETE", { late: false });
    await page.click("#delete-ok");
    await env.server.waitHeld(1);
  }

  async function finishDeletion(page) {
    await env.server.release();
    await page.waitFor(
      `!document.getElementById("gate").hidden && !document.getElementById("gate-deleted").hidden`,
      { describe: "consent gate with the deleted notice after the deletion finished" },
    );
    assert.equal(await page.hidden(DELETE_BOX), true, "the dialog must be closed once the deletion finished");
  }

  test("T09 cancel cannot close the dialog while the deletion is in flight [F303-13]", async () => {
    await withPage(async (page) => {
      await startDeletionAndHold(page);
      await page.click("#delete-cancel");
      await page.settle();
      assert.equal(
        await page.hidden(DELETE_BOX),
        false,
        "the dialog must stay open while deletion is in flight, but cancel closed it (hidden=true)",
      );
      await finishDeletion(page);
    });
  });

  test("T09b Escape closes an idle dialog but not one with a deletion in flight [F303-13]", async () => {
    await withPage(async (page) => {
      await openDeleteDialog(page);
      // Self-proof: Escape does close the dialog when nothing is running, so the later "stays open" means something.
      await page.key("Escape");
      assert.equal(await page.hidden(DELETE_BOX), true, "self-proof failed: Escape did not close the idle dialog (hidden=false)");
      await page.click("#delete-open");
      await page.waitFor(`!document.querySelector("${DELETE_BOX}").hidden`, { describe: "delete dialog reopened" });
      await env.server.delay("/account", "DELETE", { late: false });
      await page.click("#delete-ok");
      await env.server.waitHeld(1);
      await page.key("Escape");
      await page.settle();
      assert.equal(await page.hidden(DELETE_BOX), false, "Escape closed the dialog while the deletion was in flight (hidden=true)");
      await finishDeletion(page);
    });
  });
});
