// LIFF 統計頁：身分只靠 LINE 的 ID token（後端驗證），使用者資料一律用 textContent 寫入 DOM.
(function () {
  "use strict";

  var liffId = document.body.dataset.liffId;
  var statusEl = document.getElementById("status");
  var bootEl = document.getElementById("boot-status");

  // [F30-20] 告知內容版本：只有告知內容真的改了才換字串（換了所有人都會重看一次）.
  var PRIVACY_VERSION = "2026-11-10";
  var CONSENT_KEY = "consentVersion";
  var THEME_KEY = "theme";

  var state = {
    days: 7,
    date: todayString(),
    month: todayString().slice(0, 7),
    inDay: false,
    tab: "today",
    summary: null,
    base: 0,
    consented: false,
    started: false,
    synced: false,
    quake: false,
    weather: null,
    regionsData: null,
    profileDirty: false,
    gen: 0, // [F302-05] 刪除帳號的世代：刪除前送出的回應回來時世代已變，一律丟棄
  };

  function el(tag, cls, text) {
    var node = document.createElement(tag);
    if (cls) node.className = cls;
    if (text !== undefined) node.textContent = text;
    return node;
  }

  function svgEl(tag, attrs) {
    var node = document.createElementNS("http://www.w3.org/2000/svg", tag);
    Object.keys(attrs).forEach(function (k) { node.setAttribute(k, attrs[k]); });
    return node;
  }

  function num(v) {
    return Math.round(v).toLocaleString("zh-TW");
  }

  function grams(v) {
    return (Math.round(v * 10) / 10).toString();
  }

  function todayString() {
    // 台灣日期（UTC+8，無日光節約）.
    return new Date(Date.now() + 8 * 3600 * 1000).toISOString().slice(0, 10);
  }

  function shiftDate(date, delta) {
    var d = new Date(date + "T00:00:00Z");
    d.setUTCDate(d.getUTCDate() + delta);
    return d.toISOString().slice(0, 10);
  }

  function showStatus(text, isError) {
    statusEl.hidden = !text;
    statusEl.textContent = text || "";
    statusEl.classList.toggle("error", !!isError);
  }

  function showBoot(text, isError) {
    bootEl.hidden = !text;
    bootEl.textContent = text || "";
    bootEl.classList.toggle("error", !!isError);
  }

  // ---- [F30-26] 深淺色：預設深色，只有存了 "light" 才轉淺色；不看系統設定 ----
  function readTheme() {
    try {
      var v = localStorage.getItem(THEME_KEY);
      return v === "light" ? "light" : "dark"; // 其他值（含沒設、無效值）一律深色
    } catch (e) {
      return "dark"; // 讀不到就用預設
    }
  }

  function applyTheme(theme) {
    if (theme === "light") {
      document.documentElement.dataset.theme = "light";
    } else {
      delete document.documentElement.dataset.theme;
    }
    var btn = document.getElementById("theme-toggle");
    if (btn) btn.setAttribute("aria-checked", String(theme !== "light"));
  }

  function saveTheme(theme) {
    try {
      localStorage.setItem(THEME_KEY, theme);
    } catch (e) {
      // 寫不進去仍已切換畫面，只是下次打開不記得.
    }
  }

  function initTheme() {
    applyTheme(readTheme());
    var btn = document.getElementById("theme-toggle");
    if (!btn) return;
    btn.addEventListener("click", function () {
      var next = btn.getAttribute("aria-checked") === "true" ? "light" : "dark";
      applyTheme(next);
      saveTheme(next);
    });
  }

  // ---- [F30-20] 個資告知同意（只存在手機 localStorage，後端不記）----
  function hasConsent() {
    try {
      return localStorage.getItem(CONSENT_KEY) === PRIVACY_VERSION;
    } catch (e) {
      return false; // 讀不到一律當作未同意
    }
  }

  function saveConsent() {
    try {
      localStorage.setItem(CONSENT_KEY, PRIVACY_VERSION);
      return localStorage.getItem(CONSENT_KEY) === PRIVACY_VERSION;
    } catch (e) {
      return false; // 存不進去不得放行
    }
  }

  function clearConsent() {
    try {
      localStorage.removeItem(CONSENT_KEY);
    } catch (e) {
      // 清不掉也要在本次畫面回到關卡：state.consented 另外歸零.
    }
  }

  function api(path, options) {
    // [F30-20] 未同意：任何 /me/* 都不送出（含會讓後端存名稱 / email 的 sync）.
    if (!state.consented) {
      return Promise.reject(new Error("no-consent"));
    }
    options = options || {};
    var token = liff.getIDToken();
    if (!token) {
      return Promise.reject(new Error("no-token"));
    }
    var headers = { Authorization: "Bearer " + token };
    if (options.body) headers["Content-Type"] = "application/json";
    return fetch("/api/v1/me" + path, { method: options.method || "GET", headers: headers, body: options.body })
      .then(function (res) {
        if (res.status === 401) {
          liff.login({ redirectUri: location.href });
          throw new Error("auth");
        }
        if (!res.ok) {
          // [F293-03] 錯誤帶上後端的錯誤碼（status.code）：同一個狀態碼底下的不同情況，呼叫端才分得出來.
          return res.json().catch(function () { return null; }).then(function (b) {
            var e = new Error("http-" + res.status);
            e.code = b && b.status ? b.status.code : "";
            throw e;
          });
        }
        return res.status === 204 ? null : res.json();
      });
  }

  // ---- 各區獨立失敗：錯誤只顯示在該區，內容保留 ----
  function setRegionError(id, message, retry) {
    var box = document.getElementById(id);
    box.replaceChildren();
    box.hidden = !message;
    if (!message) return;
    box.append(el("span", "", message));
    if (retry) {
      var b = el("button", "link", "重試");
      b.type = "button";
      b.addEventListener("click", retry);
      box.append(b);
    }
  }

  // ---- 今日 / 單日共用的圓環＋三格＋營養素（p = "today" 或 "dv"）----
  function renderToday(s) {
    renderDayCard("today", s.today, s.base);
    document.getElementById("profile-hint").hidden = s.base > 0;
  }

  function renderDayCard(p, t, base) {
    var byId = function (suffix) { return document.getElementById(p + "-" + suffix); };
    byId("intake").textContent = num(t.intake);
    byId("burned").textContent = num(t.burned);
    var ring = byId("ring-fill");
    var circumference = 2 * Math.PI * 50;
    var hasBase = base > 0;
    var ratio = hasBase ? Math.min(t.intake / (base + t.burned), 1) : (t.intake > 0 ? 1 : 0);
    ring.style.strokeDasharray = circumference;
    ring.style.strokeDashoffset = circumference * (1 - ratio);
    byId("ring-sub").textContent =
      hasBase ? "維持體重約 " + num(base + t.burned) + " 大卡" : "大卡已攝取";
    byId("base").textContent = hasBase ? num(base) : "—";
    var net = t.intake - base - t.burned;
    byId("net").textContent = hasBase ? (net > 0 ? "+" : "") + num(net) : "—";
    byId("net-label").textContent = hasBase ? (net < 0 ? "熱量赤字" : "熱量盈餘") : "大卡";

    var macros = byId("macros");
    macros.replaceChildren();
    var m = t.macros;
    var kcal = { protein: m.protein_g * 4, carbs: m.carbs_g * 4, fat: m.fat_g * 9 };
    var total = kcal.protein + kcal.carbs + kcal.fat;
    // [F302-04] 顏色走 class（CSS 變數隨主題變），不寫死色碼.
    [["蛋白質", m.protein_g, kcal.protein, "macro-protein"], ["碳水化合物", m.carbs_g, kcal.carbs, "macro-carbs"], ["脂肪", m.fat_g, kcal.fat, "macro-fat"]]
      .forEach(function (row) {
        var box = el("div", "macro");
        var head = el("div", "macro-head");
        head.append(el("b", "", row[0]));
        var pct = total > 0 ? Math.round(row[2] / total * 100) : 0;
        head.append(el("span", "", grams(row[1]) + " g（" + pct + "%）"));
        var bar = el("div", "bar");
        var fill = el("i", row[3]);
        fill.style.width = pct + "%";
        bar.append(fill);
        box.append(head, bar);
        macros.append(box);
      });
  }

  // ---- 趨勢（自己畫 SVG 長條圖）----
  function renderTrend(s) {
    var chart = document.getElementById("chart");
    chart.replaceChildren();
    var days = s.days;
    var W = 340, H = 200, padL = 34, padR = 16, padB = 22, padT = 8;
    var max = s.base;
    days.forEach(function (d) { max = Math.max(max, d.intake, d.burned); });
    // 刻度取整數級距（250 / 500 / 1000…），避免 625、1875 這種難讀的數字.
    var step = [100, 250, 500, 1000, 2000, 5000].find(function (n) { return max / n <= 5; }) || 10000;
    var ticks = Math.max(2, Math.ceil(max / step));
    max = ticks * step;
    var svg = svgEl("svg", { viewBox: "0 0 " + W + " " + H, role: "img", "aria-label": "每日攝取與運動消耗長條圖" });
    var plotW = W - padL - padR, plotH = H - padB - padT;
    var y = function (v) { return padT + plotH - (v / max) * plotH; };
    for (var g = 0; g <= ticks; g++) {
      var v = step * g;
      svg.append(svgEl("line", { x1: padL, x2: W - padR, y1: y(v), y2: y(v), class: "grid-line" }));
      var label = svgEl("text", { x: padL - 4, y: y(v) + 3, "text-anchor": "end" });
      label.textContent = Math.round(v);
      svg.append(label);
    }
    var slot = plotW / days.length;
    var barW = Math.max(2, Math.min(14, slot / 2 - 1));
    days.forEach(function (d, i) {
      var x = padL + i * slot + slot / 2;
      svg.append(svgEl("rect", { x: x - barW, y: y(d.intake), width: barW, height: plotH + padT - y(d.intake), class: "chart-intake", rx: 2 }));
      svg.append(svgEl("rect", { x: x, y: y(d.burned), width: barW, height: plotH + padT - y(d.burned), class: "chart-burned", rx: 2 }));
      var every = days.length > 10 ? 5 : 1;
      if (i % every === 0 || i === days.length - 1) {
        var anchor = i === days.length - 1 && days.length > 10 ? "end" : "middle";
        var t = svgEl("text", { x: i === days.length - 1 && days.length > 10 ? W - 2 : x, y: H - 6, "text-anchor": anchor });
        t.textContent = d.date.slice(5).replace("-", "/");
        svg.append(t);
      }
    });
    if (s.base > 0) {
      svg.append(svgEl("line", { x1: padL, x2: W - padR, y1: y(s.base), y2: y(s.base), class: "chart-base", "stroke-width": 1.5, "stroke-dasharray": "4 3" }));
    }
    document.getElementById("legend-base").hidden = !(s.base > 0);
    chart.append(svg);

    var intake = 0, burned = 0, run = 0, logged = 0;
    days.forEach(function (d) { intake += d.intake; burned += d.burned; run += d.runKm; if (d.meals > 0) logged++; });
    var stats = document.getElementById("trend-stats");
    stats.replaceChildren();
    [["平均攝取", logged ? num(intake / logged) : "—", "大卡／有記錄的天"], ["運動消耗", num(burned), "大卡"], ["跑步", grams(run), "公里"]]
      .forEach(function (row) {
        var box = el("div", "stat");
        box.append(el("span", "", row[0]), el("strong", "", row[1]), el("small", "", row[2]));
        stats.append(box);
      });
  }

  // ---- 紀錄 ----
  function timeOf(iso) {
    return new Date(iso).toLocaleTimeString("zh-TW", { hour: "2-digit", minute: "2-digit", hour12: false, timeZone: "Asia/Taipei" });
  }

  function deleteButton(label, onDelete) {
    var b = el("button", "", "刪除");
    b.type = "button";
    b.setAttribute("aria-label", "刪除 " + label);
    b.addEventListener("click", function () {
      if (!confirm("確定刪除「" + label + "」？")) return;
      var gen = state.gen;
      b.disabled = true;
      onDelete()
        .then(function () {
          if (gen !== state.gen) return;
          // [F293-01] 刪除已經成功：重新載入失敗只說讀取失敗，不能落進下面的刪除失敗分支.
          loadAll().catch(function () { showStatus("讀取失敗，請稍後再試", true); });
        })
        .catch(function () { b.disabled = false; showStatus("刪除失敗，請稍後再試", true); });
    });
    return b;
  }

  var MEAL_TYPES = { breakfast: "早餐", lunch: "午餐", snack: "點心", dinner: "晚餐", late_night: "宵夜" };
  var PLACE_SOURCES = { dish: "（推測）", photo: "（照片辨識）" };
  var SCALES = [0.5, 0.75, 1, 1.25, 1.5, 2];

  // mealMeta 時間・餐別・打字估算.
  function mealMeta(m) {
    var parts = [timeOf(m.eatenAt)];
    if (MEAL_TYPES[m.mealType]) parts.push(MEAL_TYPES[m.mealType]);
    if (m.source === "text") parts.push("打字估算");
    return parts.join("・");
  }

  // placeLine 店家（單獨一行，店名才不會被折成兩半）；沒有時回傳空字串.
  function placeLine(m) {
    if (!m.place || !(m.place.name || m.place.address)) return "";
    return "📍 " + (m.place.name || m.place.address) + (PLACE_SOURCES[m.place.source] || "");
  }

  // scaleSelect 份量（相對於 AI 估算的倍數）；改了立即存檔.
  function scaleSelect(m, label) {
    var current = m.scale || 1;
    var wrap = el("label", "scale");
    var select = el("select");
    select.setAttribute("aria-label", label + " 的份量");
    var values = SCALES.indexOf(current) < 0 ? SCALES.concat([current]).sort(function (a, b) { return a - b; }) : SCALES;
    values.forEach(function (v) {
      var opt = el("option", "", "×" + v);
      opt.value = String(v);
      opt.selected = v === current;
      select.append(opt);
    });
    select.addEventListener("change", function () {
      var gen = state.gen;
      select.disabled = true;
      api("/meals/" + encodeURIComponent(m.id), { method: "PATCH", body: JSON.stringify({ scale: Number(select.value) }) })
        .then(function () {
          if (gen !== state.gen) return;
          // [F293-01] 份量已經改好：重新載入失敗只說讀取失敗，不能把選單改回舊值、說修改失敗；選單留在新值並恢復可選.
          loadAll().catch(function () { select.disabled = false; showStatus("讀取失敗，請稍後再試", true); });
        })
        .catch(function (err) {
          if (err && err.message === "http-409") {
            // 同一筆剛在聊天裡被修改（例如加了品項）：重新載入最新內容，再請使用者重選
            showStatus("這筆剛在其他地方被修改，已重新載入，請再選一次份量", true);
            if (gen === state.gen) loadAll().catch(function () { showStatus("讀取失敗，請稍後再試", true); });
            return;
          }
          select.value = String(current);
          select.disabled = false;
          showStatus("份量修改失敗，請稍後再試", true);
        });
    });
    wrap.append(el("span", "", "份量"), select);
    return wrap;
  }

  // 「紀錄」分頁與月曆單日檢視吃同一份 /day 資料、共用 state.date.
  function renderDay(day) {
    renderRecordLists(day, { label: "day-label", next: "day-next", meals: "meal-list", workouts: "workout-list" });
    renderRecordLists(day, { label: "dv-label", next: "dv-next", meals: "dv-meal-list", workouts: "dv-workout-list" });
    var intake = 0, burned = 0, p = 0, c = 0, f = 0;
    day.meals.forEach(function (m) {
      intake += m.total.calories; p += m.total.protein_g; c += m.total.carbs_g; f += m.total.fat_g;
    });
    day.workouts.forEach(function (w) { burned += w.calories; });
    renderDayCard("dv", { intake: intake, burned: burned, macros: { protein_g: p, carbs_g: c, fat_g: f } }, state.base);
  }

  function renderRecordLists(day, ids) {
    document.getElementById(ids.label).textContent = day.date.replace(/-/g, "/");
    document.getElementById(ids.next).disabled = day.date >= todayString();
    var meals = document.getElementById(ids.meals);
    meals.replaceChildren();
    if (day.meals.length === 0) meals.append(el("li", "empty", "這天沒有飲食紀錄"));
    day.meals.forEach(function (m) {
      var names = m.items.map(function (i) { return i.name; }).join("、");
      var li = el("li", "item");
      var body = el("div", "item-body");
      body.append(el("div", "item-title", names));
      body.append(el("div", "item-meta", mealMeta(m)));
      var place = placeLine(m);
      if (place) body.append(el("div", "item-meta", place));
      body.append(el("div", "item-meta", "蛋白質 " + grams(m.total.protein_g) + "g・碳水 " +
        grams(m.total.carbs_g) + "g・脂肪 " + grams(m.total.fat_g) + "g"));
      body.append(scaleSelect(m, names));
      li.append(body, el("div", "item-kcal", num(m.total.calories) + " 大卡"),
        deleteButton(names, function () { return api("/meals/" + encodeURIComponent(m.id), { method: "DELETE" }); }));
      meals.append(li);
    });
    var workouts = document.getElementById(ids.workouts);
    workouts.replaceChildren();
    if (day.workouts.length === 0) workouts.append(el("li", "empty", "這天沒有運動紀錄"));
    day.workouts.forEach(function (w) {
      var desc = w.activity + (w.durationMin ? " " + num(w.durationMin) + " 分鐘" : "") + (w.distanceKm ? "・" + grams(w.distanceKm) + " 公里" : "");
      var li = el("li", "item");
      var body = el("div", "item-body");
      body.append(el("div", "item-title", desc), el("div", "item-meta", timeOf(w.doneAt)));
      li.append(body, el("div", "item-kcal", num(w.calories) + " 大卡"),
        deleteButton(desc, function () { return api("/workouts/" + encodeURIComponent(w.id), { method: "DELETE" }); }));
      workouts.append(li);
    });
  }

  // ---- 設定 ----
  function fillProfile(p) {
    // 使用者正在編輯、還沒儲存時，回前景重載不覆寫他輸入的值.
    if (state.profileDirty) return;
    var f = document.getElementById("profile-form");
    f.weightKg.value = p.weightKg || "";
    f.heightCm.value = p.heightCm || "";
    f.age.value = p.age || "";
    f.querySelectorAll('input[name="sex"]').forEach(function (r) { r.checked = r.value === p.sex; });
  }

  document.getElementById("profile-form").addEventListener("input", function () { state.profileDirty = true; });

  function saveProfile(e) {
    e.preventDefault();
    var f = e.target;
    var sex = f.querySelector('input[name="sex"]:checked');
    var body = JSON.stringify({
      weightKg: Number(f.weightKg.value) || 0,
      heightCm: Number(f.heightCm.value) || 0,
      age: Number(f.age.value) || 0,
      sex: sex ? sex.value : "",
    });
    var gen = state.gen;
    api("/profile", { method: "PUT", body: body })
      .then(function () {
        if (gen !== state.gen) return;
        state.profileDirty = false;
        showStatus("已儲存");
        // [F293-01] 儲存已經成功：重新載入失敗只說讀取失敗，不能落進下面的儲存失敗分支.
        loadAll().catch(function () { showStatus("讀取失敗，請稍後再試", true); });
      })
      .catch(function (err) {
        // [F291-09][F292-06] 410 = 儲存送出後使用者刪除了資料，伺服器已補刪：不能顯示「已儲存」，也不是數值不合理；並重新載入，畫面跟著清空.
        if (err && err.message === "http-410") {
          showStatus("你的資料已經刪除，這筆沒有儲存", true);
          state.profileDirty = false; // 資料已被刪除，畫面要跟著清空，不保留輸入
          if (gen === state.gen) loadAll().catch(function () { showStatus("讀取失敗，請稍後再試", true); });
          return;
        }
        // [F293-03] 儲存在刪除之後才落地、補刪又失敗（5002）：照實說刪除沒做成，與聊天的回覆相同.
        if (err && err.code === "5002") {
          showStatus("部分資料刪除失敗，請稍後再試一次", true);
          return;
        }
        // [F292-06] 只有數值被後端拒絕（400）才說數值不合理；伺服器與網路錯誤要說稍後再試.
        if (err && err.message === "http-400") {
          showStatus("儲存失敗，請確認數值是否合理", true);
          return;
        }
        showStatus("儲存失敗，請稍後再試", true);
      });
  }

  document.getElementById("profile-form").addEventListener("submit", saveProfile);

  // ---- 載入（各區獨立失敗：任一區失敗只在該區顯示錯誤，其他區照常）----
  var NO_TOKEN_MESSAGE = "無法取得登入資訊：請確認 LIFF 設定已開啟 openid 權限";

  function runSection(task, errorIds, message, retry) {
    var gen = state.gen;
    return task().then(function () {
      if (gen !== state.gen) return true; // 刪除帳號前發出的回應：當作無事，不寫畫面
      errorIds.forEach(function (id) { setRegionError(id, ""); });
      return true;
    }, function (err) {
      if (gen !== state.gen) return true;
      var text = err && err.message === "no-token" ? NO_TOKEN_MESSAGE : message;
      errorIds.forEach(function (id) { setRegionError(id, text, retry); });
      return false;
    });
  }

  function loadSummary() {
    var gen = state.gen;
    return api("/summary?days=" + state.days).then(function (s) {
      if (gen !== state.gen) return;
      state.summary = s;
      state.base = s.base;
      renderToday(s);
      renderTrend(s);
      fillProfile(s.profile);
    });
  }

  var daySeq = 0;
  // 只有 /day 成功才更新 state.date：切日失敗時標題與內容都維持原本那一天.
  function loadDay(date) {
    var seq = ++daySeq;
    var gen = state.gen;
    return api("/day?date=" + date).then(function (day) {
      if (gen !== state.gen) return;
      if (seq !== daySeq) return;
      state.date = date;
      renderDay(day);
    });
  }

  var monthSeq = 0;
  function loadMonth() {
    var seq = ++monthSeq;
    var gen = state.gen;
    var grid = document.getElementById("cal-grid");
    renderMonthNav();
    if (!grid.children.length) grid.append(el("p", "empty cal-loading", "載入中…"));
    return api("/month?month=" + state.month).then(function (m) {
      if (gen !== state.gen) return;
      if (seq !== monthSeq) return;
      if (!m || !Array.isArray(m.days)) throw new Error("bad-month");
      if (typeof m.base === "number") state.base = m.base;
      renderMonth(m);
    }).catch(function (err) {
      if (gen !== state.gen) throw err;
      if (seq === monthSeq) grid.querySelectorAll(".cal-loading").forEach(function (n) { n.remove(); });
      throw err;
    });
  }

  function loadAlerts() {
    var gen = state.gen;
    return api("/alerts").then(function (v) {
      if (gen !== state.gen) return;
      var subs = v && Array.isArray(v.subscriptions) ? v.subscriptions : [];
      state.quake = false;
      state.weather = null;
      subs.forEach(function (s) {
        if (s.alertType === "earthquake_tsunami") state.quake = true;
        if (s.alertType === "weather_warning") {
          state.weather = Array.isArray(s.regions) ? s.regions.filter(function (r) { return typeof r === "string"; }) : [];
        }
      });
      document.getElementById("alerts-list").hidden = false;
      renderAlerts();
    }, function (err) {
      if (gen !== state.gen) throw err;
      // 讀取失敗 = 不知道現況：隱藏開關列表與空狀態提示，不把「不知道」顯示成「已關閉」.
      document.getElementById("alerts-list").hidden = true;
      document.getElementById("alerts-empty").hidden = true;
      throw err;
    });
  }

  function loadAccount() {
    // 第一次用 sync（存名稱 / 頭像 / email 並回傳）；sync 被擋（例如剛刪除資料）就退回讀取.
    var gen = state.gen;
    var first = state.synced ? api("/account") : api("/account/sync", { method: "POST" }).catch(function () { return api("/account"); });
    return first.then(function (a) {
      if (gen !== state.gen) return;
      state.synced = true;
      renderAccount(a);
    });
  }

  var READ_FAILED = "讀取失敗，請稍後再試";
  var SECTIONS = {
    summary: { run: loadSummary, errors: ["today-error", "trend-error"], message: READ_FAILED },
    day: { run: function () { return loadDay(state.date); }, errors: ["records-error", "dv-error"], message: READ_FAILED },
    month: { run: loadMonth, errors: ["cal-error"], message: READ_FAILED },
    alerts: { run: loadAlerts, errors: ["alerts-error"], message: "通知讀取失敗，請稍後再試" },
    account: { run: loadAccount, errors: ["account-error"], message: "會員資料讀取失敗，請稍後再試" },
  };

  function runNamed(name) {
    var s = SECTIONS[name];
    return runSection(s.run, s.errors, s.message, function () { runNamed(name); });
  }

  // 全部重載：任一區失敗時 promise reject（呼叫端只說「讀取失敗」）；各區錯誤已顯示在自己的區塊.
  function loadAll() {
    return runAllSections();
  }

  function runAllSections() {
    return Promise.all(Object.keys(SECTIONS).map(runNamed)).then(function (results) {
      if (results.indexOf(false) >= 0) throw new Error("partial");
      if (statusEl.textContent === "載入中…") showStatus("");
    });
  }

  function goDay(date) {
    return runSection(function () { return loadDay(date); }, ["records-error", "dv-error"], READ_FAILED, function () { goDay(date); });
  }

  // ---- [F30-23] 月曆 ----
  var GRADE_CLASS = { green: "grade-green", yellow: "grade-yellow", red: "grade-red" };

  function shiftMonth(month, delta) {
    var parts = month.split("-");
    var idx = Number(parts[0]) * 12 + (Number(parts[1]) - 1) + delta;
    var y = Math.floor(idx / 12);
    var m = idx % 12 + 1;
    return y + "-" + (m < 10 ? "0" : "") + m;
  }

  function renderMonthNav() {
    var parts = state.month.split("-");
    document.getElementById("month-label").textContent = parts[0] + " 年 " + Number(parts[1]) + " 月";
    document.getElementById("month-next").disabled = state.month >= todayString().slice(0, 7);
    document.getElementById("month-prev").disabled = state.month <= "2020-01";
  }

  function calendarCell(d, hasBase) {
    var b = el("button", "cal-cell");
    b.type = "button";
    var md = d.date.slice(5).split("-");
    var label = Number(md[0]) + "月" + Number(md[1]) + "日";
    var meals = d.meals > 0, runs = d.workouts > 0;
    // [F302-07] 等級的第二通道：角落符號（給眼睛）與 aria-label 等級文字（給螢幕閱讀器）；與上色同受 gradeOK 管轄.
    var gradeOK = hasBase && meals;
    var GRADE_MARK = { green: "−", yellow: "≈", red: "+" };
    var GRADE_WORD = { green: "熱量赤字", yellow: "接近平衡", red: "盈餘偏多" };
    b.append(el("span", "cal-day", String(Number(md[1]))));
    if (meals) b.append(el("span", "cal-intake", num(d.intake)));
    if (runs) b.append(el("span", "cal-run", "▲ " + num(d.burned)));
    // 顏色只看後端給的 grade；沒填身體資料或當天沒有飲食紀錄不上色；未知值不上色.
    if (gradeOK && Object.prototype.hasOwnProperty.call(GRADE_CLASS, d.grade)) b.classList.add(GRADE_CLASS[d.grade]);
    if (gradeOK && Object.prototype.hasOwnProperty.call(GRADE_MARK, d.grade)) {
      var mark = el("span", "cal-grade", GRADE_MARK[d.grade]);
      mark.setAttribute("aria-hidden", "true");
      b.append(mark);
    }
    if (d.date === todayString()) {
      b.classList.add("today");
      b.setAttribute("aria-current", "date");
    }
    var parts = [label];
    if (meals) parts.push("攝取 " + num(d.intake) + " 大卡");
    if (runs) parts.push("運動消耗 " + num(d.burned) + " 大卡");
    if (!meals && !runs) parts.push("沒有紀錄");
    if (gradeOK && Object.prototype.hasOwnProperty.call(GRADE_WORD, d.grade)) parts.push(GRADE_WORD[d.grade]);
    b.setAttribute("aria-label", parts.join(" "));
    b.addEventListener("click", function () { openDay(d.date); });
    return b;
  }

  function renderMonth(m) {
    var grid = document.getElementById("cal-grid");
    grid.replaceChildren();
    renderMonthNav();
    var hasBase = state.base > 0;
    document.getElementById("cal-profile-hint").hidden = hasBase;
    if (m.days.length === 0) return;
    // 週日為第一欄.
    var lead = new Date(m.days[0].date + "T00:00:00Z").getUTCDay();
    for (var i = 0; i < lead; i++) grid.append(el("span", "cal-blank"));
    m.days.forEach(function (d) { grid.append(calendarCell(d, hasBase)); });
  }

  function setMonth(month) {
    if (month !== state.month) document.getElementById("cal-grid").replaceChildren();
    state.month = month;
    runNamed("month");
  }

  function openDay(date) {
    runSection(function () { return loadDay(date); }, ["cal-error", "records-error", "dv-error"], READ_FAILED, function () { openDay(date); })
      .then(function (ok) {
        if (!ok) return;
        state.inDay = true;
        selectTab("calendar");
      });
  }

  document.getElementById("month-prev").addEventListener("click", function () { setMonth(shiftMonth(state.month, -1)); });
  document.getElementById("month-next").addEventListener("click", function () {
    if (state.month >= todayString().slice(0, 7)) return;
    setMonth(shiftMonth(state.month, 1));
  });
  document.getElementById("dv-back").addEventListener("click", function () {
    state.inDay = false;
    selectTab("calendar");
    setMonth(state.date.slice(0, 7)); // 跨月切日後，返回的是該日所在月份；同時刷新格內數字
  });

  // ---- [F30-24] 單日換日（失敗有 catch：標題與內容維持原本那一天）----
  function stepDay(delta) {
    if (delta > 0 && state.date >= todayString()) return;
    goDay(shiftDate(state.date, delta));
  }
  document.getElementById("day-prev").addEventListener("click", function () { stepDay(-1); });
  document.getElementById("day-next").addEventListener("click", function () { stepDay(1); });
  document.getElementById("dv-prev").addEventListener("click", function () { stepDay(-1); });
  document.getElementById("dv-next").addEventListener("click", function () { stepDay(1); });

  // ---- [F30-21] 會員資料 ----
  function isHttpsUrl(u) {
    return typeof u === "string" && /^https:\/\//i.test(u);
  }

  function renderAccount(a) {
    a = a || {};
    var name = typeof a.displayName === "string" && a.displayName ? a.displayName : "LINE 使用者";
    var email = typeof a.email === "string" && a.email ? a.email : "未提供";
    document.getElementById("account-name").textContent = name;
    document.getElementById("account-email").textContent = email;
    var box = document.getElementById("account-avatar");
    box.replaceChildren();
    var initial = el("span", "avatar-initial", Array.from(name)[0] || "?");
    if (isHttpsUrl(a.pictureUrl)) {
      var img = el("img");
      img.alt = "";
      img.referrerPolicy = "no-referrer";
      img.addEventListener("error", function () { box.replaceChildren(initial); });
      img.src = a.pictureUrl;
      box.append(img);
    } else {
      box.append(initial);
    }
  }

  var deleteBox = document.getElementById("delete-confirm");
  // [F302-03] 對話框（刪除確認、地區選擇）共用的開 / 關：焦點鎖在面板內、Escape 等同取消、關閉後焦點回開啟鈕、背景 inert.
  var openBox = null; // 目前開著的面板（同時只會有一個）
  var openOpener = null; // 開啟它的按鈕：由呼叫端明確傳入（WebKit 點按鈕不聚焦，不能讀 document.activeElement）
  var dialogBackground = [document.querySelector("#app > header.top"), document.querySelector("#app > main")];
  document.getElementById("delete-title").tabIndex = -1;
  document.getElementById("picker-title").tabIndex = -1;

  function focusablesIn(box) {
    return Array.prototype.filter.call(box.querySelectorAll("button, [href], input, select, textarea, [tabindex]"), function (n) {
      return !n.disabled && !n.hidden && n.getAttribute("tabindex") !== "-1";
    });
  }

  function openDialog(box, firstFocus, opener) {
    openBox = box;
    openOpener = opener;
    dialogBackground.forEach(function (n) { if (n) n.inert = true; });
    box.hidden = false;
    firstFocus.focus();
  }

  function closeDialog(box, restore) {
    if (openBox !== box) return;
    box.hidden = true;
    dialogBackground.forEach(function (n) { if (n) n.inert = false; });
    var opener = openOpener;
    openBox = null;
    openOpener = null;
    if (!restore) return;
    var usable = opener && document.contains(opener) && !opener.disabled && !opener.closest("[hidden]");
    var fallback = box === deleteBox ? document.getElementById("delete-open") : weatherSwitch;
    (usable ? opener : fallback).focus();
  }

  document.addEventListener("keydown", function (e) {
    if (!openBox) return;
    if (e.key === "Escape") {
      e.preventDefault();
      if (openBox === deleteBox) {
        if (!document.getElementById("delete-ok").disabled) closeDialog(deleteBox, true);
      } else if (!picker.submitting) {
        closePicker();
      }
      return;
    }
    if (e.key !== "Tab") return;
    var items = focusablesIn(openBox);
    if (items.length === 0) {
      e.preventDefault();
      return;
    }
    var at = items.indexOf(document.activeElement);
    if (at < 0 || (e.shiftKey && at === 0) || (!e.shiftKey && at === items.length - 1)) {
      e.preventDefault();
      (e.shiftKey ? items[items.length - 1] : items[0]).focus();
    }
  });
  var deleteMsg = document.getElementById("delete-msg");

  document.getElementById("delete-open").addEventListener("click", function () {
    deleteMsg.hidden = true;
    deleteMsg.textContent = "";
    document.getElementById("delete-ok").disabled = false;
    document.getElementById("delete-cancel").disabled = false; // 上一次刪除成功後取消鈕停在 disabled，重開面板要能按
    openDialog(deleteBox, document.getElementById("delete-cancel"), document.getElementById("delete-open"));
  });
  document.getElementById("delete-cancel").addEventListener("click", function () { closeDialog(deleteBox, true); });
  document.getElementById("delete-ok").addEventListener("click", function () {
    var ok = document.getElementById("delete-ok");
    // 焦點在按鈕上時 disabled 會把焦點丟回 body，先移到標題.
    document.getElementById("delete-title").focus();
    ok.disabled = true;
    document.getElementById("delete-cancel").disabled = true; // [F302-10] 請求在途不准取消（否則面板關了、刪除照樣完成）；失敗再解鎖
    api("/account", { method: "DELETE" })
      .then(afterAccountDeleted)
      .catch(function (err) {
        ok.disabled = false;
        document.getElementById("delete-cancel").disabled = false;
        if (openBox === deleteBox) ok.focus();
        // 失敗不清本機同意、不清畫面.
        deleteMsg.textContent = err && err.code === "5002" ? "部分資料刪除失敗，請稍後再試一次" : "刪除失敗，請稍後再試";
        deleteMsg.hidden = false;
      });
  });

  function resetDayCard(p) {
    var byId = function (suffix) { return document.getElementById(p + "-" + suffix); };
    byId("intake").textContent = "0";
    byId("burned").textContent = "0";
    byId("base").textContent = "—";
    byId("net").textContent = "—";
    byId("net-label").textContent = "大卡";
    byId("ring-sub").textContent = "大卡已攝取";
    byId("macros").replaceChildren();
    byId("ring-fill").style.strokeDashoffset = 2 * Math.PI * 50;
  }

  // 刪除成功（以 API 成功為準）：先清本機同意，再清空所有畫面資料，回到告知關卡.
  function afterAccountDeleted() {
    state.gen++;
    clearConsent();
    state.consented = false;
    state.started = false;
    state.synced = false;
    state.summary = null;
    state.base = 0;
    state.quake = false;
    state.weather = null;
    state.profileDirty = false;
    state.inDay = false;
    state.date = todayString();
    state.month = todayString().slice(0, 7);
    resetDayCard("today");
    resetDayCard("dv");
    ["chart", "trend-stats", "meal-list", "workout-list", "dv-meal-list", "dv-workout-list", "cal-grid"].forEach(function (id) {
      document.getElementById(id).replaceChildren();
    });
    ["today-error", "trend-error", "records-error", "dv-error", "cal-error", "alerts-error", "account-error"].forEach(function (id) {
      setRegionError(id, "");
    });
    document.getElementById("account-name").textContent = "";
    document.getElementById("account-email").textContent = "";
    document.getElementById("account-avatar").replaceChildren();
    document.getElementById("profile-form").reset();
    renderAlerts();
    setAlertsMessage("");
    showStatus("");
    closeDialog(deleteBox, false);
    closePicker(false);
    showGate({ deleted: true });
  }

  // ---- [F30-22] 通知 ----
  var quakeSwitch = document.getElementById("alert-quake-switch");
  var weatherSwitch = document.getElementById("alert-weather-switch");
  var weatherEdit = document.getElementById("alert-weather-edit");

  function setAlertsMessage(text, isError) {
    var box = document.getElementById("alerts-msg");
    box.hidden = !text;
    box.textContent = text || "";
    box.classList.toggle("error", !!isError);
  }

  function renderAlerts() {
    quakeSwitch.setAttribute("aria-checked", String(state.quake));
    var weatherOn = state.weather !== null;
    weatherSwitch.setAttribute("aria-checked", String(weatherOn));
    weatherEdit.textContent = weatherOn ? "修改地區" : "選擇地區";
    var regions = document.getElementById("alert-weather-regions");
    regions.replaceChildren();
    if (weatherOn) {
      state.weather.forEach(function (r, i) {
        if (i > 0) regions.append("、");
        regions.append(el("span", "region-name", r)); // 地區字串一律 textContent
      });
    }
    regions.hidden = !weatherOn;
    document.getElementById("alerts-empty").hidden = state.quake || weatherOn;
  }

  // 開啟類寫入失敗的文案：410 = 送出期間資料被刪除，5002 = 補刪失敗，其餘 = 一般失敗.
  function enableFailureText(err) {
    if (err && err.message === "http-410") return "你的資料已經刪除，這筆沒有開啟";
    if (err && err.code === "5002") return "部分資料刪除失敗，請稍後再試一次";
    return "開啟失敗，請稍後再試";
  }

  var REOPEN_HINT = "已關閉。要再開啟，可在這裡打開開關，或在聊天室輸入「開啟天氣特報」「開啟地震海嘯警告」";

  quakeSwitch.addEventListener("click", function () {
    var turningOff = state.quake;
    var gen = state.gen;
    quakeSwitch.disabled = true;
    setAlertsMessage("");
    var call = turningOff
      ? api("/alerts/earthquake_tsunami", { method: "DELETE" })
      : api("/alerts/earthquake_tsunami", { method: "PUT" });
    call.then(function () {
      quakeSwitch.disabled = false;
      if (gen !== state.gen) return; // [F303-01] 刪帳號前送出的請求，回應晚到不改 state、不重畫、不顯示提示（先解鎖，開關才不會卡死）
      state.quake = !turningOff;
      renderAlerts();
      if (turningOff) setAlertsMessage(REOPEN_HINT);
    }).catch(function (err) {
      quakeSwitch.disabled = false; // 失敗：開關維持原狀
      if (turningOff) {
        setAlertsMessage("關閉失敗，請稍後再試", true);
        return;
      }
      setAlertsMessage(enableFailureText(err), true);
      if (err && err.message === "http-410" && gen === state.gen) runAllSections().catch(function () { showStatus(READ_FAILED, true); });
    });
  });

  weatherSwitch.addEventListener("click", function () {
    setAlertsMessage("");
    if (state.weather === null) {
      openPicker([], weatherSwitch);
      return;
    }
    // 關閉天氣特報 = 該類全部地區一起關.
    var gen = state.gen;
    weatherSwitch.disabled = true;
    api("/alerts/weather_warning", { method: "DELETE" })
      .then(function () {
        weatherSwitch.disabled = false;
        if (gen !== state.gen) return; // [F303-01] 同 quakeSwitch：刪帳號後的舊回應丟棄
        state.weather = null;
        renderAlerts();
        setAlertsMessage(REOPEN_HINT);
      })
      .catch(function () {
        weatherSwitch.disabled = false;
        setAlertsMessage("關閉失敗，請稍後再試", true);
      });
  });
  weatherEdit.addEventListener("click", function () {
    setAlertsMessage("");
    openPicker(state.weather || [], weatherEdit);
  });

  // ---- 地區選擇器（縣市 → 整個縣市或鄉鎮；字串照 /regions 原樣送回，不做字形轉換、不自備清單）----
  var pickerBox = document.getElementById("picker");
  var picker = { entries: [], view: "counties", county: null, draft: null, current: [], prefilled: false, submitting: false, error: false };

  function regionsOf(entries) {
    var out = [];
    entries.forEach(function (e) {
      if (e.raw !== undefined) out.push(e.raw);
      else if (e.whole) out.push(e.county);
      else e.towns.forEach(function (t) { out.push(e.county + t); });
    });
    return out;
  }

  function entryFor(entries, county) {
    for (var i = 0; i < entries.length; i++) if (entries[i].county === county) return entries[i];
    return null;
  }

  // 把目前已開啟的地區字串還原成「整個縣市 / 鄉鎮」選擇；認不得的字串原樣保留，不默默丟掉.
  function entriesFrom(regions, counties) {
    var out = [];
    regions.forEach(function (r) {
      var whole = counties.filter(function (c) { return c.name === r; })[0];
      if (whole) {
        out = out.filter(function (e) { return e.county !== whole.name; });
        out.push({ county: whole.name, whole: true, towns: [] });
        return;
      }
      var hit = null;
      counties.forEach(function (c) {
        c.townships.forEach(function (t) { if (c.name + t === r) hit = { county: c.name, town: t }; });
      });
      if (!hit) {
        out.push({ raw: r });
        return;
      }
      var e = entryFor(out, hit.county);
      if (!e) {
        e = { county: hit.county, whole: false, towns: [] };
        out.push(e);
      }
      if (!e.whole && e.towns.indexOf(hit.town) < 0) e.towns.push(hit.town);
    });
    return out;
  }

  function openPicker(current, opener) {
    picker.entries = [];
    picker.view = "counties";
    picker.county = null;
    picker.draft = null;
    picker.current = current;
    picker.prefilled = false;
    picker.submitting = false;
    picker.error = false;
    setPickerMessage("");
    renderPicker();
    openDialog(pickerBox, pickerFocusTarget(), opener);
    ensureRegions();
  }

  // restore 只有明確傳 false（刪帳號後切回關卡）才不還原焦點；按鈕 click 傳進來的是 Event，視為要還原.
  function closePicker(restore) {
    closeDialog(pickerBox, restore !== false);
    picker.entries = []; // 取消 / 關閉：已選項目丟棄，再開時從頭來
    picker.draft = null;
    picker.submitting = false;
  }

  function setPickerMessage(text) {
    var box = document.getElementById("picker-msg");
    box.hidden = !text;
    box.textContent = text || "";
  }

  function ensureRegions() {
    if (state.regionsData) {
      afterRegionsLoaded();
      return;
    }
    picker.error = false;
    renderPicker();
    api("/regions").then(function (v) {
      var list = v && Array.isArray(v.counties) ? v.counties : null;
      if (!list || list.length === 0) throw new Error("bad-regions");
      state.regionsData = list.map(function (c) {
        return { name: String(c.name), townships: Array.isArray(c.townships) ? c.townships.map(String) : [] };
      });
      if (!pickerBox.hidden) afterRegionsLoaded();
    }).catch(function () {
      if (pickerBox.hidden) return;
      picker.error = true; // 失敗顯示錯誤與重試，不把空清單當成「沒有縣市」
      renderPicker();
    });
  }

  function afterRegionsLoaded() {
    if (!picker.prefilled) {
      picker.prefilled = true;
      picker.entries = entriesFrom(picker.current, state.regionsData);
    }
    renderPicker();
  }

  function pickerButton(text, cls, onClick) {
    var b = el("button", cls, text);
    b.type = "button";
    b.addEventListener("click", onClick);
    return b;
  }

  // 面板內第一個可聚焦元素；沒有就用標題（tabIndex = -1，只供程式聚焦）.
  function pickerFocusTarget() {
    return focusablesIn(pickerBox)[0] || document.getElementById("picker-title");
  }

  // 重繪會換掉節點，焦點若在被換掉的節點上就掉回 body；面板開著時把它拉回面板內.
  function ensurePickerFocus() {
    if (openBox === pickerBox && !pickerBox.contains(document.activeElement)) pickerFocusTarget().focus();
  }

  function renderPicker() {
    var body = document.getElementById("picker-body");
    var chips = document.getElementById("picker-chips");
    var actions = document.getElementById("picker-actions");
    body.replaceChildren();
    chips.replaceChildren();
    actions.replaceChildren();
    var editing = state.weather !== null;
    document.getElementById("picker-title").textContent = picker.view === "county" ? picker.county : (editing ? "修改地區" : "選擇地區");

    if (!state.regionsData) {
      if (picker.error) {
        body.append(el("p", "region-error", "地區清單讀取失敗"), pickerButton("重試", "link", ensureRegions));
      } else {
        body.append(el("p", "empty", "載入中…"));
      }
      actions.append(pickerButton("取消", "secondary", closePicker));
      ensurePickerFocus();
      return;
    }

    if (picker.view === "county") {
      renderCountyView(body, actions);
      ensurePickerFocus();
      return;
    }

    var list = el("div", "picker-list");
    state.regionsData.forEach(function (c) {
      var e = entryFor(picker.entries, c.name);
      var mark = e ? (e.whole ? "（整個縣市）" : "（" + e.towns.length + " 個鄉鎮）") : "";
      list.append(pickerButton(c.name + mark, "picker-county", function () { openCounty(c); }));
    });
    body.append(list);

    regionsOf(picker.entries).forEach(function (r) {
      var chip = el("span", "chip");
      chip.append(el("span", "", r));
      var rm = pickerButton("×", "chip-x", function () { removeRegion(r); });
      rm.setAttribute("aria-label", "移除 " + r);
      chip.append(rm);
      chips.append(chip);
    });

    var cancel = pickerButton("取消", "secondary", closePicker);
    cancel.disabled = picker.submitting;
    var submit = pickerButton(editing ? "儲存地區" : "開啟通知", "primary", submitPicker);
    submit.disabled = picker.submitting || regionsOf(picker.entries).length === 0;
    actions.append(cancel, submit);
    ensurePickerFocus();
  }

  function removeRegion(r) {
    var next = [];
    picker.entries.forEach(function (e) {
      if (e.raw !== undefined) {
        if (e.raw !== r) next.push(e);
      } else if (e.whole) {
        if (e.county !== r) next.push(e);
      } else {
        e.towns = e.towns.filter(function (t) { return e.county + t !== r; });
        if (e.towns.length > 0) next.push(e);
      }
    });
    picker.entries = next;
    renderPicker();
  }

  function openCounty(c) {
    var e = entryFor(picker.entries, c.name);
    picker.view = "county";
    picker.county = c.name;
    picker.draft = e ? { whole: e.whole, towns: e.towns.slice() } : { whole: false, towns: [] };
    setPickerMessage("");
    renderPicker();
  }

  function renderCountyView(body, actions) {
    var c = state.regionsData.filter(function (x) { return x.name === picker.county; })[0];
    var draft = picker.draft;
    var boxes = [];
    var wholeBox = el("input");
    wholeBox.type = "checkbox";
    wholeBox.checked = draft.whole;
    var wholeLabel = el("label", "check");
    wholeLabel.append(wholeBox, el("span", "", "整個縣市"));
    var list = el("div", "picker-list");
    list.append(wholeLabel);
    c.townships.forEach(function (t) {
      var box = el("input");
      box.type = "checkbox";
      box.checked = draft.towns.indexOf(t) >= 0;
      box.addEventListener("change", function () {
        if (box.checked) {
          draft.whole = false;
          wholeBox.checked = false;
          if (draft.towns.indexOf(t) < 0) draft.towns.push(t);
        } else {
          draft.towns = draft.towns.filter(function (x) { return x !== t; });
        }
      });
      boxes.push(box);
      var label = el("label", "check");
      label.append(box, el("span", "", t));
      list.append(label);
    });
    // 整個縣市與該縣市的鄉鎮互斥.
    wholeBox.addEventListener("change", function () {
      draft.whole = wholeBox.checked;
      if (wholeBox.checked) {
        draft.towns = [];
        boxes.forEach(function (b) { b.checked = false; });
      }
    });
    body.append(list);
    actions.append(
      pickerButton("返回", "secondary", function () { picker.view = "counties"; picker.draft = null; renderPicker(); }),
      pickerButton("確定", "primary", function () {
        // 同縣市再選會覆蓋先前的選擇；什麼都沒勾 = 移除該縣市.
        var rest = picker.entries.filter(function (e) { return e.county !== picker.county; });
        var idx = picker.entries.indexOf(entryFor(picker.entries, picker.county));
        if (draft.whole || draft.towns.length > 0) {
          var entry = { county: picker.county, whole: draft.whole, towns: draft.towns.slice() };
          if (idx >= 0) rest.splice(idx, 0, entry);
          else rest.push(entry);
        }
        picker.entries = rest;
        picker.view = "counties";
        picker.draft = null;
        renderPicker();
      })
    );
  }

  function submitPicker() {
    var regions = regionsOf(picker.entries);
    if (regions.length === 0 || picker.submitting) return;
    picker.submitting = true;
    setPickerMessage("");
    renderPicker();
    var gen = state.gen;
    api("/alerts/weather_warning", { method: "PUT", body: JSON.stringify({ regions: regions }) })
      .then(function (item) {
        if (gen !== state.gen) return; // [F303-01] 刪帳號時 afterAccountDeleted 已關面板；舊回應不得關掉刪除後新開的面板
        // 以伺服器回的 regions 為準（會正規化），不是自己送出的陣列.
        state.weather = item && Array.isArray(item.regions) ? item.regions.filter(function (r) { return typeof r === "string"; }) : regions;
        closePicker();
        renderAlerts();
      })
      .catch(function (err) {
        if (gen !== state.gen) return; // [F303-01] 舊回應不動新面板與提示
        picker.submitting = false;
        renderPicker(); // 面板與已選項目保留，可重按
        if (err && err.message === "http-410") {
          closePicker();
          setAlertsMessage("你的資料已經刪除，這筆沒有開啟", true);
          if (gen === state.gen) runAllSections().catch(function () { showStatus(READ_FAILED, true); });
          return;
        }
        setPickerMessage(enableFailureText(err));
      });
  }

  // ---- [F30-20] 告知關卡 ----
  var gateEl = document.getElementById("gate");
  var appEl = document.getElementById("app");

  function setHidden(id, hidden) {
    document.getElementById(id).hidden = hidden;
  }

  function showGate(opts) {
    opts = opts || {};
    showBoot("");
    appEl.hidden = true;
    gateEl.hidden = false;
    setHidden("gate-deleted", !opts.deleted);
    setHidden("gate-msg", true);
    setHidden("gate-declined", true);
    setHidden("gate-close", true);
    setHidden("gate-consent-actions", !!opts.readonly);
    setHidden("gate-readonly-actions", !opts.readonly);
    window.scrollTo(0, 0);
  }

  function showApp() {
    showBoot("");
    gateEl.hidden = true;
    appEl.hidden = false;
  }

  function begin() {
    if (!liff.isLoggedIn()) {
      showBoot("登入中…");
      liff.login({ redirectUri: location.href });
      return Promise.resolve();
    }
    showApp();
    selectTab("today");
    showStatus("載入中…");
    state.started = true;
    lastReload = Date.now();
    var done = function () { if (statusEl.textContent === "載入中…") showStatus(""); };
    return loadAll().then(done, done); // 各區自己顯示錯誤
  }

  document.getElementById("consent-agree").addEventListener("click", function () {
    if (!saveConsent()) {
      var msg = document.getElementById("gate-msg");
      msg.textContent = "無法儲存同意狀態，請稍後再試";
      msg.hidden = false;
      return; // 存不進去不放行
    }
    state.consented = true;
    begin();
  });
  document.getElementById("consent-decline").addEventListener("click", function () {
    // 不寫入同意值、不打任何 /me/*；停在關卡，可繼續閱讀.
    setHidden("gate-declined", false);
    setHidden("gate-close", false);
  });
  document.getElementById("gate-close").addEventListener("click", function () {
    if (typeof liff !== "undefined" && liff.closeWindow) liff.closeWindow();
  });
  document.getElementById("privacy-open").addEventListener("click", function () { showGate({ readonly: true }); });
  document.getElementById("privacy-back").addEventListener("click", showApp);

  // ---- 分頁 ----
  function selectTab(name) {
    state.tab = name;
    // [F302-06] 分頁是導覽：目前頁用 aria-current="page"，其餘移除.
    document.querySelectorAll(".tab").forEach(function (t) {
      if (t.dataset.tab === name) {
        t.setAttribute("aria-current", "page");
      } else {
        t.removeAttribute("aria-current");
      }
    });
    document.querySelectorAll(".panel").forEach(function (p) {
      var target = name === "calendar" ? (state.inDay ? "dayview" : "calendar") : name;
      p.hidden = p.id !== target;
    });
  }

  document.querySelectorAll(".tab").forEach(function (t) {
    t.addEventListener("click", function () { selectTab(t.dataset.tab); });
  });
  function reloadTrend() {
    var gen = state.gen;
    return runSection(function () {
      return api("/summary?days=" + state.days).then(function (s) { if (gen !== state.gen) return; state.summary = s; renderTrend(s); });
    }, ["trend-error"], READ_FAILED, reloadTrend);
  }
  document.querySelectorAll(".seg").forEach(function (b) {
    b.addEventListener("click", function () {
      state.days = Number(b.dataset.days);
      document.querySelectorAll(".seg").forEach(function (x) { x.setAttribute("aria-pressed", String(x === b)); });
      reloadTrend();
    });
  });

  // ---- [F30-25] 回前景重載：visibilitychange 與 pageshow 都掛（WKWebView 的實際觸發時機未驗證）----
  var lastReload = 0;
  var reloading = false;
  function reloadOnForeground() {
    if (!state.consented || !state.started || reloading) return;
    var now = Date.now();
    if (now - lastReload < 5000) return; // 節流：兩次重載至少隔 5 秒
    lastReload = now;
    reloading = true;
    runAllSections()
      .then(function () {
        if (statusEl.classList.contains("error") && statusEl.textContent === READ_FAILED) showStatus("");
      }, function () {
        showStatus(READ_FAILED, true); // 只顯示讀取失敗，不清畫面
      })
      .then(function () { reloading = false; });
  }
  document.addEventListener("visibilitychange", function () {
    if (document.visibilityState === "visible") reloadOnForeground();
  });
  window.addEventListener("pageshow", function (e) {
    if (e.persisted) reloadOnForeground();
  });

  initTheme(); // 先套主題，關卡 / 錯誤畫面也用使用者選的顏色

  if (typeof liff === "undefined") {
    showBoot("請從 LINE 開啟這個頁面", true);
    return;
  }
  liff.init({ liffId: liffId })
    .then(function () {
      state.consented = hasConsent();
      if (!state.consented) {
        showGate({});
        return null;
      }
      return begin();
    })
    .catch(function () {
      showBoot("載入失敗，請稍後再試", true);
    });
})();
