// [F303-12] Smoke test: the real page loads through the fake server with the fake LIFF SDK injected.
// [F303-15] Rendering checks: button contrast, no third-party fonts before consent, alerts read failure, calendar grades, switch colour.
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { after, before, test } from "node:test";
import { readCsp, repoRoot, startEnv } from "./lib/harness.mjs";

let env;

before(async () => {
  env = await startEnv();
});

after(async () => {
  await env?.stop();
});

// The one smoke check, used twice: on a normal tab it must pass, on a tab without the SDK stand-in it must fail.
async function smokeCheck(page) {
  await page.goto("/liff");
  assert.equal(
    page.documentHeaders?.["content-security-policy"],
    readCsp(),
    "the page must be served with the product's own Content-Security-Policy, unchanged",
  );
  const hasLiff = await page.eval('typeof window.liff === "object" && typeof window.__e2eLiff === "object"');
  assert.ok(hasLiff, `window.liff is missing: the fake SDK was not injected; blockedRequests=${JSON.stringify(page.blockedRequests)}`);
  assert.equal(
    page.blockedRequests.filter((u) => u.endsWith("/sdk.js")).length,
    0,
    `the SDK request was blocked instead of answered: ${JSON.stringify(page.blockedRequests)}`,
  );
  await page.waitFor('!document.getElementById("gate").hidden', { describe: "consent gate visible" });
  assert.deepEqual(page.cspViolations, [], "the page must raise no Content-Security-Policy violation");
}

test("smoke: real page, real CSP, fake SDK, consent gate shown [F303-12]", async () => {
  const page = await env.newPage();
  try {
    await smokeCheck(page);
  } finally {
    await page.close();
  }
});

test("smoke control: without the SDK stand-in the smoke check goes red [F303-12]", async () => {
  const page = await env.newPage({ fakeSdk: false });
  try {
    await assert.rejects(
      () => smokeCheck(page),
      /window\.liff is missing|SDK request was blocked/,
      "smoke check passed although the SDK interception was off: it would not notice a broken harness",
    );
    assert.ok(
      page.blockedRequests.some((u) => u.endsWith("/sdk.js")),
      `the SDK URL must show up in blockedRequests, got ${JSON.stringify(page.blockedRequests)}`,
    );
  } finally {
    await page.close();
  }
});

async function withPage(body) {
  const page = await env.newPage();
  try {
    await body(page);
  } finally {
    await page.close();
  }
}

// WCAG 2.x relative luminance and contrast ratio for opaque sRGB colours given as "rgb(r, g, b)".
function parseRgb(css) {
  const m = /^rgba?\((\d+), (\d+), (\d+)(?:, ([\d.]+))?\)$/.exec(css);
  if (!m) throw new Error(`cannot parse colour ${JSON.stringify(css)}`);
  // [F31-37] A translucent colour has no single contrast value (it depends on what is behind it): refuse instead of treating it as opaque.
  if (m[4] !== undefined && Number(m[4]) < 1) throw new Error(`translucent colour ${JSON.stringify(css)}: contrast needs an opaque colour`);
  return [Number(m[1]), Number(m[2]), Number(m[3])];
}

function luminance(css) {
  const [r, g, b] = parseRgb(css).map((v) => {
    const c = v / 255;
    return c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4;
  });
  return 0.2126 * r + 0.7152 * g + 0.0722 * b;
}

function contrast(a, b) {
  const [hi, lo] = [luminance(a), luminance(b)].sort((x, y) => y - x);
  return (hi + 0.05) / (lo + 0.05);
}

// [F31-37] No browser needed: the helper itself must refuse what it cannot measure.
test("contrast helper refuses translucent colours [F31-37]", () => {
  assert.throws(() => contrast("rgba(255, 255, 255, 0.5)", "rgb(0, 0, 0)"), /translucent.*rgba\(255, 255, 255, 0\.5\)/);
  assert.throws(() => contrast("rgb(0, 0, 0)", "rgba(10, 20, 30, 0)"), /translucent/);
  assert.ok(Math.abs(contrast("rgb(255, 255, 255)", "rgb(0, 0, 0)") - 21) < 1e-9, "opaque rgb() is measured");
  assert.ok(Math.abs(contrast("rgba(255, 255, 255, 1)", "rgb(0, 0, 0)") - 21) < 1e-9, "rgba() with alpha 1 is opaque");
});

test("T05 the primary button in the default dark theme has readable contrast [F303-15]", async () => {
  await withPage(async (page) => {
    await page.goto("/liff");
    await page.waitFor('!document.getElementById("gate").hidden', { describe: "consent gate visible" });
    const theme = await page.eval("document.documentElement.dataset.theme === undefined ? 'dark' : document.documentElement.dataset.theme");
    assert.equal(theme, "dark", "precondition: the page starts in the dark theme");
    const { bg, fg } = await page.eval(`(() => {
      const cs = getComputedStyle(document.getElementById("consent-agree"));
      return { bg: cs.backgroundColor, fg: cs.color };
    })()`);
    const ratio = contrast(fg, bg);
    assert.ok(ratio >= 4.5, `#consent-agree text ${fg} on background ${bg} has contrast ${ratio.toFixed(2)}:1, below 4.5:1`);
  });
});

test("T06 no font service is contacted before consent [F303-15]", async () => {
  await withPage(async (page) => {
    await page.goto("/liff");
    await page.waitFor('!document.getElementById("gate").hidden', { describe: "consent gate visible" });
    await page.settle();
    const fontHost = /fonts\.(googleapis|gstatic)\.com/;
    const attempted = [...page.blockedRequests, ...page.requests].filter((u) => fontHost.test(u));
    const links = await page.eval('document.querySelectorAll(\'link[href*="fonts.g"]\').length');
    assert.equal(
      attempted.length + links,
      0,
      `${links} font link(s) in the page and ${attempted.length} request(s) to a font service before consent: ${JSON.stringify([...new Set(attempted)])}`,
    );
    const consented = await page.eval('localStorage.getItem("consentVersion")');
    assert.equal(consented, null, "precondition: no consent was given in this tab");
  });
});

test("T10 alert switches are hidden when the alerts cannot be read [F303-15]", async () => {
  await withPage(async (page) => {
    await env.server.fail("/alerts", "GET", 500);
    await page.openApp();
    await page.click('.tab[data-tab="settings"]');
    await page.waitFor('document.getElementById("alerts-error").textContent !== ""', { describe: "alerts error text" });
    assert.equal(await page.hidden("#alerts-error"), false, "the alerts error box must be visible");
    const text = await page.text("#alerts-error");
    assert.ok(text.includes("通知讀取失敗"), `the alerts error must say the notifications could not be read, got "${text}"`);
    assert.equal(await page.hidden("#alerts-list"), true, "#alerts-list must be hidden when alerts fail to load (unknown state is not shown as off)");
    for (const id of ["alert-quake-switch", "alert-weather-switch"]) {
      const shown = await page.eval(`document.getElementById("${id}").offsetParent !== null`);
      assert.equal(shown, false, `#${id} must not be operable while the alert state is unknown`);
    }
  });
});

const MONTH_GOLDEN = JSON.parse(readFileSync(join(repoRoot, "internal/liff/testdata/month.json"), "utf8"));

// The golden month with four days rewritten to exercise every grade branch, including the negative ones.
function monthWithGrades() {
  const month = structuredClone(MONTH_GOLDEN);
  const set = (date, patch) => Object.assign(month.days.find((d) => d.date === date), patch);
  set("2026-11-03", { grade: "yellow", meals: 1, intake: 1800 });
  set("2026-11-04", { grade: "red", meals: 1, intake: 2600 });
  set("2026-11-05", { grade: "purple", meals: 1, intake: 2000 }); // unknown grade: no colour, no mark
  set("2026-11-06", { grade: "yellow", meals: 0, intake: 0 }); // no meals logged: no colour, no mark
  return month;
}

function calendarCell(page, day) {
  return page.eval(`(() => {
    const cell = [...document.querySelectorAll("#cal-grid .cal-cell")].find((c) => c.querySelector(".cal-day").textContent === ${JSON.stringify(String(day))});
    if (!cell) return null;
    const mark = cell.querySelector(".cal-grade");
    return {
      classes: cell.className,
      mark: mark ? mark.textContent : null,
      markAriaHidden: mark ? mark.getAttribute("aria-hidden") : null,
      label: cell.getAttribute("aria-label"),
    };
  })()`);
}

test("T11 calendar cells carry the grade colour and corner mark; unknown grade and no-meal days stay plain [F303-15]", async () => {
  await withPage(async (page) => {
    page.override((url) => url.pathname === "/api/v1/me/month", monthWithGrades());
    await page.openApp();
    await page.click('.tab[data-tab="calendar"]');
    await page.waitFor('document.querySelectorAll("#cal-grid .cal-cell").length === 30', { describe: "30 calendar cells" });
    const graded = [
      { day: 2, cls: "grade-green", mark: "−", word: "熱量赤字" },
      { day: 3, cls: "grade-yellow", mark: "≈", word: "接近平衡" },
      { day: 4, cls: "grade-red", mark: "+", word: "盈餘偏多" },
    ];
    for (const want of graded) {
      const cell = await calendarCell(page, want.day);
      assert.ok(cell, `11-0${want.day}: calendar cell not found`);
      assert.ok(cell.classes.split(" ").includes(want.cls), `11-0${want.day}: expected class ${want.cls}, got "${cell.classes}"`);
      assert.equal(cell.mark, want.mark, `11-0${want.day}: corner mark must be "${want.mark}", got ${JSON.stringify(cell.mark)}`);
      assert.equal(cell.markAriaHidden, "true", `11-0${want.day}: the corner mark must be aria-hidden (the spoken grade is in aria-label)`);
      assert.ok(cell.label.includes(want.word), `11-0${want.day}: aria-label must speak the grade "${want.word}", got "${cell.label}"`);
    }
    for (const [day, why] of [[5, "unknown grade"], [6, "no meals logged"]]) {
      const cell = await calendarCell(page, day);
      assert.ok(cell, `11-0${day}: calendar cell not found`);
      assert.equal(cell.classes, "cal-cell", `11-0${day} (${why}) must not be coloured, got class "${cell.classes}"`);
      assert.equal(cell.mark, null, `11-0${day} (${why}) must not show a corner mark, got ${JSON.stringify(cell.mark)}`);
    }
  });
});

// [F31-16] The switch must be readable by contrast, not by one hard-coded colour: when on, the white knob against the fill
// and the fill against the card behind it are both at least 3:1 (WCAG 1.4.11), in the dark and the light theme.
async function switchContrast(page) {
  return page.eval(`(() => {
    const sw = document.getElementById("alert-quake-switch");
    let card = sw.parentElement;
    while (card && /^rgba\\(.*, 0\\)$|^transparent$/.test(getComputedStyle(card).backgroundColor)) card = card.parentElement;
    return {
      theme: document.documentElement.dataset.theme === undefined ? "dark" : document.documentElement.dataset.theme,
      fill: getComputedStyle(sw).backgroundColor,
      knob: getComputedStyle(sw, "::after").backgroundColor,
      card: card ? getComputedStyle(card).backgroundColor : "none",
    };
  })()`);
}

test("T12 the earthquake switch keeps 3:1 contrast on and off: knob on track and track on card, both themes [F31-16][F31-34]", async () => {
  await withPage(async (page) => {
    await env.server.setAlerts(false, null);
    await page.openApp();
    await page.click('.tab[data-tab="settings"]');
    const problems = [];
    // The dark theme is the page default and carries no data-theme value, so "dark" is read as "no value".
    const themeExpr = 'document.documentElement.dataset.theme === undefined ? "dark" : document.documentElement.dataset.theme';
    async function useTheme(theme) {
      if ((await page.eval(themeExpr)) === theme) return;
      await page.click("#theme-toggle");
      await page.waitFor(`(${themeExpr}) === ${JSON.stringify(theme)}`, { describe: `${theme} theme applied` });
    }
    // [F31-34] The off state is measured first (the switch starts off), then the switch is turned on and measured again.
    async function measure(state, theme) {
      await useTheme(theme);
      const v = await switchContrast(page);
      assert.equal(v.theme, theme, "precondition: the expected theme is active");
      const knobOnFill = contrast(v.knob, v.fill);
      const fillOnCard = contrast(v.fill, v.card);
      console.log(`# T12 ${state} ${theme}: knob ${v.knob} on fill ${v.fill} = ${knobOnFill.toFixed(2)}, fill on card ${v.card} = ${fillOnCard.toFixed(2)}`);
      if (state === "on") {
        if (knobOnFill < 3) problems.push(`${theme}: the knob ${v.knob} on the switch fill ${v.fill} has contrast ${knobOnFill.toFixed(2)}:1, below 3:1`);
        if (fillOnCard < 3) problems.push(`${theme}: the switch fill ${v.fill} on the card ${v.card} has contrast ${fillOnCard.toFixed(2)}:1, below 3:1`);
      } else {
        if (knobOnFill < 3) problems.push(`${theme} (off): the knob ${v.knob} on the track ${v.fill} has contrast ${knobOnFill.toFixed(2)}:1, below 3:1`);
        if (fillOnCard < 3) problems.push(`${theme} (off): the track ${v.fill} on the card ${v.card} has contrast ${fillOnCard.toFixed(2)}:1, below 3:1`);
      }
    }
    assert.equal(await page.attr("#alert-quake-switch", "aria-checked"), "false", "precondition: the earthquake switch starts off");
    for (const theme of ["dark", "light"]) await measure("off", theme);
    await page.click("#alert-quake-switch");
    await page.waitFor(
      'document.getElementById("alert-quake-switch").getAttribute("aria-checked") === "true" && !document.getElementById("alert-quake-switch").disabled',
      { describe: "earthquake switch on and enabled" },
    );
    for (const theme of ["light", "dark"]) await measure("on", theme);
    assert.deepEqual(problems, [], problems.join(" | "));
  });
});

// [F31-15] The member card no longer has an email line: nothing in the page asks for or shows an email.
test("T13 the member card has no email line and the notice does not announce email collection [F31-15]", async () => {
  await withPage(async (page) => {
    await page.openApp();
    await page.click('.tab[data-tab="settings"]');
    const view = await page.eval(`(() => ({
      emailEl: document.getElementById("account-email") !== null,
      cardText: document.getElementById("account-card").textContent,
      noticeMentions: /email/i.test(document.getElementById("privacy-notice").textContent),
    }))()`);
    assert.equal(view.emailEl, false, "#account-email must not exist in the DOM");
    assert.ok(!/@|email/i.test(view.cardText), `the member card must not show an email, got ${JSON.stringify(view.cardText)}`);
    assert.equal(view.noticeMentions, false, "the privacy notice must not mention email");
  });
});

// [F31-11] Guard (the colours already pass): the theme toggle knob must stand out from its track in both themes (WCAG 1.4.11, 3:1).
async function knobContrast(page) {
  const view = await page.eval(`(() => ({
    theme: document.documentElement.dataset.theme === undefined ? "dark" : document.documentElement.dataset.theme,
    knob: getComputedStyle(document.querySelector("#theme-toggle .knob")).backgroundColor,
    track: getComputedStyle(document.getElementById("theme-toggle")).backgroundColor,
  }))()`);
  return { ...view, ratio: contrast(view.knob, view.track) };
}

test("T14-knob-dark the theme toggle knob has at least 3:1 against its track in the dark theme [F31-11]", async () => {
  await withPage(async (page) => {
    await page.goto("/liff");
    await page.waitFor('!document.getElementById("gate").hidden', { describe: "consent gate visible" });
    const r = await knobContrast(page);
    assert.equal(r.theme, "dark", "precondition: the page starts in the dark theme");
    assert.ok(r.ratio >= 3, `dark theme knob ${r.knob} on track ${r.track} has contrast ${r.ratio.toFixed(2)}:1, below 3:1`);
    console.log(`# knob contrast dark=${r.ratio.toFixed(2)}`);
  });
});

test("T14-knob-light the theme toggle knob has at least 3:1 against its track in the light theme [F31-11]", async () => {
  await withPage(async (page) => {
    await page.goto("/liff");
    await page.waitFor('!document.getElementById("gate").hidden', { describe: "consent gate visible" });
    await page.click("#theme-toggle");
    await page.waitFor('document.documentElement.dataset.theme === "light"', { describe: "light theme applied" });
    const r = await knobContrast(page);
    assert.ok(r.ratio >= 3, `light theme knob ${r.knob} on track ${r.track} has contrast ${r.ratio.toFixed(2)}:1, below 3:1`);
    console.log(`# knob contrast light=${r.ratio.toFixed(2)}`);
  });
});
