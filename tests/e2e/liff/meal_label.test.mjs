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
