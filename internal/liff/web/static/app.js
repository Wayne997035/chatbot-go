// LIFF 統計頁：身分只靠 LINE 的 ID token（後端驗證），使用者資料一律用 textContent 寫入 DOM.
(function () {
  "use strict";

  var liffId = document.body.dataset.liffId;
  var statusEl = document.getElementById("status");
  var state = { days: 7, date: todayString(), summary: null };

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

  function api(path, options) {
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
        if (!res.ok) throw new Error("http-" + res.status);
        return res.status === 204 ? null : res.json();
      });
  }

  // ---- 今日 ----
  function renderToday(s) {
    var t = s.today;
    document.getElementById("today-intake").textContent = num(t.intake);
    document.getElementById("today-burned").textContent = num(t.burned);
    var ring = document.getElementById("ring-fill");
    var circumference = 2 * Math.PI * 50;
    var hasBase = s.base > 0;
    var ratio = hasBase ? Math.min(t.intake / (s.base + t.burned), 1) : (t.intake > 0 ? 1 : 0);
    ring.style.strokeDasharray = circumference;
    ring.style.strokeDashoffset = circumference * (1 - ratio);
    document.getElementById("today-ring-sub").textContent =
      hasBase ? "維持體重約 " + num(s.base + t.burned) + " 大卡" : "大卡已攝取";
    document.getElementById("today-base").textContent = hasBase ? num(s.base) : "—";
    var net = t.intake - s.base - t.burned;
    document.getElementById("today-net").textContent = hasBase ? (net > 0 ? "+" : "") + num(net) : "—";
    document.getElementById("today-net-label").textContent = hasBase ? (net < 0 ? "熱量赤字" : "熱量盈餘") : "大卡";
    document.getElementById("profile-hint").hidden = hasBase;

    var macros = document.getElementById("macros");
    macros.replaceChildren();
    var m = t.macros;
    var kcal = { protein: m.protein_g * 4, carbs: m.carbs_g * 4, fat: m.fat_g * 9 };
    var total = kcal.protein + kcal.carbs + kcal.fat;
    [["蛋白質", m.protein_g, kcal.protein, "#1d6fd8"], ["碳水化合物", m.carbs_g, kcal.carbs, "#d9790b"], ["脂肪", m.fat_g, kcal.fat, "#c2335f"]]
      .forEach(function (row) {
        var box = el("div", "macro");
        var head = el("div", "macro-head");
        head.append(el("b", "", row[0]));
        var pct = total > 0 ? Math.round(row[2] / total * 100) : 0;
        head.append(el("span", "", grams(row[1]) + " g（" + pct + "%）"));
        var bar = el("div", "bar");
        var fill = el("i");
        fill.style.width = pct + "%";
        fill.style.background = row[3];
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
      svg.append(svgEl("line", { x1: padL, x2: W - padR, y1: y(v), y2: y(v), stroke: "#e2e6ec" }));
      var label = svgEl("text", { x: padL - 4, y: y(v) + 3, "text-anchor": "end" });
      label.textContent = Math.round(v);
      svg.append(label);
    }
    var slot = plotW / days.length;
    var barW = Math.max(2, Math.min(14, slot / 2 - 1));
    days.forEach(function (d, i) {
      var x = padL + i * slot + slot / 2;
      svg.append(svgEl("rect", { x: x - barW, y: y(d.intake), width: barW, height: plotH + padT - y(d.intake), fill: "#d9790b", rx: 2 }));
      svg.append(svgEl("rect", { x: x, y: y(d.burned), width: barW, height: plotH + padT - y(d.burned), fill: "#1b8a43", rx: 2 }));
      var every = days.length > 10 ? 5 : 1;
      if (i % every === 0 || i === days.length - 1) {
        var anchor = i === days.length - 1 && days.length > 10 ? "end" : "middle";
        var t = svgEl("text", { x: i === days.length - 1 && days.length > 10 ? W - 2 : x, y: H - 6, "text-anchor": anchor });
        t.textContent = d.date.slice(5).replace("-", "/");
        svg.append(t);
      }
    });
    if (s.base > 0) {
      svg.append(svgEl("line", { x1: padL, x2: W - padR, y1: y(s.base), y2: y(s.base), stroke: "#6f3fc0", "stroke-width": 1.5, "stroke-dasharray": "4 3" }));
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
      b.disabled = true;
      onDelete().then(loadAll).catch(function () { b.disabled = false; showStatus("刪除失敗，請稍後再試", true); });
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
      select.disabled = true;
      api("/meals/" + encodeURIComponent(m.id), { method: "PATCH", body: JSON.stringify({ scale: Number(select.value) }) })
        .then(loadAll)
        .catch(function (err) {
          if (err && err.message === "http-409") {
            // 同一筆剛在聊天裡被修改（例如加了品項）：重新載入最新內容，再請使用者重選
            showStatus("這筆剛在其他地方被修改，已重新載入，請再選一次份量", true);
            loadAll().catch(function () { showStatus("讀取失敗，請稍後再試", true); });
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

  function renderRecords(day) {
    document.getElementById("day-label").textContent = day.date.replace(/-/g, "/");
    document.getElementById("day-next").disabled = day.date >= todayString();
    var meals = document.getElementById("meal-list");
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
    var workouts = document.getElementById("workout-list");
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
    var f = document.getElementById("profile-form");
    f.weightKg.value = p.weightKg || "";
    f.heightCm.value = p.heightCm || "";
    f.age.value = p.age || "";
    f.querySelectorAll('input[name="sex"]').forEach(function (r) { r.checked = r.value === p.sex; });
  }

  document.getElementById("profile-form").addEventListener("submit", function (e) {
    e.preventDefault();
    var f = e.target;
    var sex = f.querySelector('input[name="sex"]:checked');
    var body = JSON.stringify({
      weightKg: Number(f.weightKg.value) || 0,
      heightCm: Number(f.heightCm.value) || 0,
      age: Number(f.age.value) || 0,
      sex: sex ? sex.value : "",
    });
    api("/profile", { method: "PUT", body: body })
      .then(function () { showStatus("已儲存"); return loadAll(); })
      .catch(function (err) {
        // [F291-09] 410 = 儲存送出後使用者刪除了資料，伺服器已補刪：不能顯示「已儲存」，也不是數值不合理.
        if (err && err.message === "http-410") {
          showStatus("你的資料已經刪除，這筆沒有儲存", true);
          return;
        }
        showStatus("儲存失敗，請確認數值是否合理", true);
      });
  });

  // ---- 載入與分頁 ----
  function loadAll() {
    return Promise.all([api("/summary?days=" + state.days), api("/day?date=" + state.date)]).then(function (r) {
      state.summary = r[0];
      renderToday(r[0]);
      renderTrend(r[0]);
      renderRecords(r[1]);
      fillProfile(r[0].profile);
      if (statusEl.textContent === "載入中…") showStatus("");
    });
  }

  function selectTab(name) {
    document.querySelectorAll(".tab").forEach(function (t) { t.setAttribute("aria-pressed", String(t.dataset.tab === name)); });
    document.querySelectorAll(".panel").forEach(function (p) { p.hidden = p.id !== name; });
  }

  document.querySelectorAll(".tab").forEach(function (t) {
    t.addEventListener("click", function () { selectTab(t.dataset.tab); });
  });
  document.querySelectorAll(".seg").forEach(function (b) {
    b.addEventListener("click", function () {
      state.days = Number(b.dataset.days);
      document.querySelectorAll(".seg").forEach(function (x) { x.setAttribute("aria-pressed", String(x === b)); });
      api("/summary?days=" + state.days).then(renderTrend);
    });
  });
  document.getElementById("day-prev").addEventListener("click", function () {
    state.date = shiftDate(state.date, -1);
    api("/day?date=" + state.date).then(renderRecords);
  });
  document.getElementById("day-next").addEventListener("click", function () {
    if (state.date >= todayString()) return;
    state.date = shiftDate(state.date, 1);
    api("/day?date=" + state.date).then(renderRecords);
  });

  if (typeof liff === "undefined") {
    showStatus("請從 LINE 開啟這個頁面", true);
    return;
  }
  liff.init({ liffId: liffId })
    .then(function () {
      if (!liff.isLoggedIn()) {
        liff.login({ redirectUri: location.href });
        return null;
      }
      selectTab("today");
      return loadAll();
    })
    .catch(function (err) {
      showStatus(err && err.message === "no-token"
        ? "無法取得登入資訊：請確認 LIFF 設定已開啟 openid 權限"
        : "載入失敗，請稍後再試", true);
    });
})();
