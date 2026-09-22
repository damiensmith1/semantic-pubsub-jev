"use strict";
(function () {
  var $ = function (id) { return document.getElementById(id); };
  var esc = function (s) {
    return String(s).replace(/[&<>"]/g, function (c) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c];
    });
  };
  var api = function (p, o) { return fetch(p, o).then(function (r) {
    if (!r.ok) return r.json().catch(function(){return {};}).then(function (j) {
      throw new Error(j.error || r.statusText);
    });
    return r.status === 204 ? null : r.json();
  }); };

  var S = {
    cfg: { topics: [], threshold: 0.5, judgeLive: false, model: "", runs: [] },
    topics: [], slots: {}, publishing: false,
    topic: null, subs: [], recs: [], thr: 0.5,
    exp: { run: null, rows: [], size: null, thr: 0.5, sel: null }
  };

  // A bank per topic, so the examples match whatever is selected. They are
  // written to straddle the seeded subscribers rather than match one
  // cleanly — a bank where every message has an obvious recipient would
  // demonstrate nothing that a keyword match could not.
  var BANK = {
    "alerts.infra": [
      "primary database volume at 96% capacity, writes will fail within the hour",
      "p99 round-trip time up 400ms to eu-west, packet loss 2%",
      "replication lag to the eu-west replica now 14 minutes and climbing",
      "checkout succeeding but 8% slower than baseline, no errors reported",
      "database failover failed, orders returning 503 to all customers",
      "memory on the ingest workers climbing steadily for six hours",
      "nightly backup completed in 41 minutes, no errors",
      "connection pool exhausted on orders-db, queries queuing",
      "disk on the log aggregator at 71%, growing 3% per day",
      "TLS handshake failures to the payments provider, 4% of attempts"
    ],
    "deploys": [
      "rolled back payments v2.8.0 after error rate tripled in canary",
      "deployed checkout-api v4.2.1, all health checks green, no downtime",
      "migration 0142 added an index to orders, 40 seconds, no lock held",
      "deploy of search v1.9 stuck, three pods crash-looping on startup",
      "migration 0143 will drop the legacy sessions table on next release",
      "canary for orders v3.3 held at 5% pending latency review",
      "config change enabled the new pricing engine for 2% of traffic",
      "build 8841 failed on the main branch, tests timed out",
      "schema change adds a nullable column to customers, no backfill",
      "hotfix deployed straight to production, skipping canary"
    ],
    "security": [
      "4,000 failed login attempts from a single ASN in ten minutes",
      "an admin role was granted to a service account outside the usual process",
      "TLS certificate for api.example.com expires in six days",
      "dependency scan found a high-severity CVE in an image built last night",
      "a support engineer read 300 customer records in five minutes",
      "MFA was disabled on two accounts in the finance group",
      "audit log shipping to the SIEM has been failing for 40 minutes",
      "a new API key was created with full account scope",
      "password reset requested for eleven accounts from one IP",
      "firewall rule opened port 5432 to 0.0.0.0/0"
    ],
    "_default": [
      "primary database volume at 96% capacity, writes will fail within the hour",
      "rolled back payments v2.8.0 after error rate tripled in canary",
      "4,000 failed login attempts from a single ASN in ten minutes",
      "p99 round-trip time up 400ms to eu-west, packet loss 2%",
      "an admin role was granted to a service account outside the usual process",
      "deployed checkout-api v4.2.1, all health checks green, no downtime"
    ]
  };
  var SHOWN = 4;

  /* ── tabs ─────────────────────────────────────────────────── */
  document.querySelector(".tabs").addEventListener("click", function (e) {
    var b = e.target.closest(".tab"); if (!b) return;
    document.querySelectorAll(".tab").forEach(function (t) {
      t.setAttribute("aria-selected", String(t === b));
    });
    ["live", "findings"].forEach(function (v) {
      $("view-" + v).hidden = v !== b.dataset.view;
    });
    if (b.dataset.view === "findings") {
      renderFindings();
      // The explorer now lives at the foot of this page, so it loads with it.
      if (!S.exp.rows.length) loadRun();
    }
  });

  /* ── live: subscribers ────────────────────────────────────── */
  // A plain sentinel, not a control character: this string becomes a NUL
  // at runtime, and a NUL in an HTML attribute value is replaced by the
  // parser with U+FFFD — so the comparison never matched and "+ new
  // topic" silently did nothing.
  var NEW_TOPIC = "__new_topic__";

  function setTopics(list) {
    var have = {}; (list || []).forEach(function (t) { have[t] = 1; });
    if (S.topic) have[S.topic] = 1;
    S.topics = Object.keys(have).sort();
    $("topic").innerHTML = S.topics.map(function (t) {
      return '<option value="' + esc(t) + '"' + (t === S.topic ? " selected" : "") + ">" + esc(t) + "</option>";
    }).join("") + '<option value="' + NEW_TOPIC + '">+ new topic\u2026</option>';
  }

  // A topic the console just created should be offered to the next visitor,
  // so re-read the discovered set after anything that could add one.
  function refreshTopics() {
    api("/api/config").then(function (cfg) { setTopics(cfg.topics || []); }).catch(function () {});
  }

  function renderSubs() {
    $("subCount").textContent = S.subs.length ? S.subs.length + " on topic" : "";
    $("subs").innerHTML = S.subs.length ? S.subs.map(function (s) {
      return "<li><div><b>" + esc(s.id) + "</b><p>" + esc(s.criteria) + "</p></div>" +
        '<button data-del="' + esc(s.id) + '" title="remove" aria-label="Remove ' + esc(s.id) + '">&times;</button></li>';
    }).join("") : '<li class="none">none yet &mdash; add one below</li>';
  }

  function loadSubs() {
    if (!S.topic) return Promise.resolve();
    return api("/api/subscribers?topic=" + encodeURIComponent(S.topic))
      .then(function (list) { S.subs = list || []; renderSubs(); })
      .catch(function (e) { S.subs = []; renderSubs(); note(e.message); });
  }

  $("addForm").addEventListener("submit", function (e) {
    e.preventDefault();
    var name = $("subName").value.trim() || ("sub-" + (S.subs.length + 1));
    var pred = $("subPred").value.trim();
    if (!pred) return;
    api("/api/subscribers", {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ topic: S.topic, id: name, criteria: pred })
    }).then(function () {
      $("subName").value = ""; $("subPred").value = "";
      refreshTopics();
      return loadSubs();
    }).catch(function (err) { note(err.message); });
  });

  $("subs").addEventListener("click", function (e) {
    var b = e.target.closest("[data-del]"); if (!b) return;
    api("/api/subscribers?id=" + encodeURIComponent(b.dataset.del), { method: "DELETE" })
      .then(loadSubs).catch(function (err) { note(err.message); });
  });

  /* ── live: publishing ─────────────────────────────────────── */
  function bankFor(topic) { return BANK[topic] || BANK._default; }

  // Which of the topic's bank are on screen, and where the next comes from.
  // Kept per topic so switching away and back does not restart the rotation.
  function slots() {
    if (!S.slots[S.topic]) {
      var n = bankFor(S.topic).length, k = Math.min(SHOWN, n);
      S.slots[S.topic] = { shown: [0, 1, 2, 3].slice(0, k), next: k % n };
    }
    return S.slots[S.topic];
  }

  function renderSamples() {
    var bank = bankFor(S.topic), st = slots();
    $("samples").innerHTML = st.shown.map(function (i, pos) {
      return '<button type="button" data-slot="' + pos + '">' + esc(bank[i]) + "</button>";
    }).join("");
  }

  // Clicking sends immediately. Filling the box and making you press a
  // second button would put a step between intent and result for no gain.
  // The slot then refills from the bank so the list never runs dry.
  $("samples").addEventListener("click", function (e) {
    var b = e.target.closest("[data-slot]");
    if (!b || S.publishing) return;
    var pos = Number(b.dataset.slot), bank = bankFor(S.topic), st = slots();
    var text = bank[st.shown[pos]];
    st.shown[pos] = st.next;
    st.next = (st.next + 1) % bank.length;
    renderSamples();
    send(text);
  });

  function send(text) {
    if (!text || !S.topic || S.publishing) return;
    S.publishing = true;
    var btn = $("pubBtn");
    btn.disabled = true; btn.textContent = "Judging…";
    api("/api/publish", {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ topic: S.topic, data: { text: text } })
    }).catch(function (err) { note(err.message); })
      .then(function () {
        S.publishing = false;
        btn.disabled = false; btn.textContent = "Publish";
      });
  }

  $("pubForm").addEventListener("submit", function (e) {
    e.preventDefault();
    var text = $("pubBody").value.trim();
    if (!text) return;
    $("pubBody").value = "";
    send(text);
  });

  /* ── live: the decision stream ────────────────────────────── */
  function body(data) {
    try {
      var o = typeof data === "string" ? JSON.parse(data) : data;
      if (o && typeof o === "object" && !Array.isArray(o)) {
        if (Object.keys(o).length === 1 && typeof o.text === "string") return esc(o.text);
        return Object.keys(o).map(function (k) {
          return '<span class="k">' + esc(k) + "</span> " + esc(String(o[k]));
        }).join("&nbsp;&nbsp;&middot;&nbsp;&nbsp;");
      }
      return esc(JSON.stringify(o));
    } catch (e) { return esc(String(data)); }
  }

  function renderStream() {
    if (!S.recs.length) {
      $("stream").innerHTML =
        '<div class="placeholder"><h3>no judgments yet</h3>' +
        "<p>Add a subscriber or two with different interests, then publish a message. " +
        "Every subscriber on the topic is judged in one request, and the decision that " +
        "actually routed the message appears here.</p></div>";
      return;
    }
    $("stream").innerHTML = S.recs.map(function (r) {
      var ds = (r.decisions || []).slice().sort(function (a, b) { return b.score - a.score; });
      var on = ds.filter(function (d) { return d.score >= S.thr; }).length;
      var lanes = ds.map(function (d) {
        var pct = Math.max(0, Math.min(1, d.score)) * 100;
        var right = pct > 62;
        return '<div class="lane ' + (d.score >= S.thr ? "on" : "") + '">' +
          '<span class="who" title="' + esc(d.criteria) + '">' + esc(d.id) + "</span>" +
          '<span class="track"><i class="pip" style="left:' + pct + '%"></i>' +
          '<span class="val" style="' + (right ? "right:" + (100 - pct) + "%;margin-right:10px"
                                               : "left:" + pct + "%;margin-left:10px") + '">' +
          d.score.toFixed(2) + "</span></span></div>";
      }).join("");
      var ticks = [0, 0.25, 0.5, 0.75, 1].map(function (t) {
        return '<span style="left:' + (t * 100) + '%">' + t.toFixed(2) + "</span>";
      }).join("");
      if (!ds.length) {
        return '<article class="rec"><div class="rec-h">' +
          '<span class="topic">' + esc(r.topic) + "</span><span>" +
          new Date(r.at).toLocaleTimeString([], {hour:"2-digit",minute:"2-digit",second:"2-digit"}) +
          "</span></div>" +
          '<p class="rec-msg">' + body(r.data) + "</p>" +
          '<div class="rec-f nobody">published &mdash; no subscribers on this topic, so nothing was judged</div></article>';
      }
      var when = new Date(r.at).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" });
      return '<article class="rec"><div class="rec-h">' +
        '<span class="topic">' + esc(r.topic) + "</span><span>" + when + "</span>" +
        "<span>" + r.inputTokens + " tok</span><span>" + Math.round(r.latencyMs) + " ms</span>" +
        "<span>" + esc(r.model) + "</span></div>" +
        '<p class="rec-msg">' + body(r.data) + "</p>" +
        '<div class="plot" style="--thr:' + S.thr + '">' + lanes +
        '<div class="axis">' + ticks + "</div></div>" +
        '<div class="rec-f"><b>' + on + "</b> of " + ds.length +
        " delivered at threshold " + S.thr.toFixed(2) + "</div></article>";
    }).join("");
  }

  function note(msg) {
    var el = $("err");
    el.textContent = msg;
    el.hidden = false;
  }

  /* ── threshold ────────────────────────────────────────────── */
  $("thr").addEventListener("input", function (e) {
    S.thr = Number(e.target.value); $("thrVal").textContent = S.thr.toFixed(2); renderStream();
  });
  $("ethr").addEventListener("input", function (e) {
    S.exp.thr = Number(e.target.value); $("ethrVal").textContent = S.exp.thr.toFixed(2); renderMatrix();
  });

  /* ── explorer ─────────────────────────────────────────────── */
  function loadRun() {
    var name = $("runSel").value; if (!name) return;
    fetch("/api/runs?name=" + encodeURIComponent(name)).then(function (r) { return r.text(); })
      .then(function (txt) {
        var per = {}, model = "", sizes = {};
        txt.split("\n").forEach(function (ln) {
          if (!ln.trim()) return;
          var r; try { r = JSON.parse(ln); } catch (e) { return; }
          model = r.model || model;
          if (r.size != null) sizes[r.size] = 1;
          var key = r.message + "\u0000" + (r.size == null ? "" : r.size);
          per[key] = per[key] || { m: r.message, n: r.size, p: {} };
          Object.keys(r.probs || {}).forEach(function (k) {
            (per[key].p[k] = per[key].p[k] || []).push(r.probs[k]);
          });
        });
        S.exp.rows = Object.keys(per).map(function (k) { return per[k]; });
        S.exp.model = model;
        var sz = Object.keys(sizes).map(Number).sort(function (a, b) { return a - b; });
        if (sz.length > 1) {
          $("sizeWrap").hidden = false;
          $("sizeSel").innerHTML = sz.map(function (n) { return "<option>" + n + "</option>"; }).join("");
          S.exp.size = sz[0]; $("sizeSel").value = String(sz[0]);
        } else { $("sizeWrap").hidden = true; S.exp.size = null; }
        S.exp.sel = null;
        renderMatrix();
      });
  }
  $("runSel").addEventListener("change", loadRun);
  $("sizeSel").addEventListener("change", function (e) {
    S.exp.size = Number(e.target.value); S.exp.sel = null; renderMatrix();
  });

  function mean(a) { var s = 0; for (var i = 0; i < a.length; i++) s += a[i]; return s / a.length; }
  function sdev(a, m) { var s = 0; for (var i = 0; i < a.length; i++) { var d = a[i] - m; s += d * d; }
                        return Math.sqrt(s / a.length); }

  function renderMatrix() {
    var rows = S.exp.rows.filter(function (r) { return S.exp.size == null || r.n === S.exp.size; });
    if (!rows.length) { $("mx").innerHTML = ""; $("expStats").textContent = ""; return; }
    var keys = {};
    rows.forEach(function (r) { Object.keys(r.p).forEach(function (k) { keys[k] = 1; }); });
    var ks = Object.keys(keys).sort();

    var flips = 0, cells = 0, near = 0, on = 0;
    var head = "<thead><tr><th>message</th>" + ks.map(function (k) {
      return "<th>" + esc(k) + "</th>"; }).join("") + "</tr></thead>";
    var bodyHtml = "<tbody>" + rows.map(function (r) {
      return "<tr><th>" + esc(r.m) + "</th>" + ks.map(function (k) {
        var ps = r.p[k];
        if (!ps) return "<td></td>";
        var mu = mean(ps), hit = mu >= S.exp.thr;
        var anyOn = false, anyOff = false;
        ps.forEach(function (p) { p >= S.exp.thr ? anyOn = true : anyOff = true; });
        cells++; if (anyOn && anyOff) flips++; if (hit) on++;
        var isNear = Math.abs(mu - S.exp.thr) < 0.05; if (isNear) near++;
        var id = r.m + "|" + k;
        return '<td class="' + (hit ? "on " : "") + (isNear ? "near " : "") +
          (S.exp.sel === id ? "sel" : "") + '"><button data-cell="' + esc(id) + '">' +
          mu.toFixed(3) + "</button></td>";
      }).join("") + "</tr>";
    }).join("") + "</tbody>";
    $("mx").innerHTML = head + bodyHtml;
    $("expStats").innerHTML =
      "<span>" + cells + " cells</span><span><b>" + on + "</b> delivered</span>" +
      "<span>" + near + " near line</span><span>" + (flips ? "<b>" + flips + "</b>" : "0") + " unstable</span>";
    renderCell();
  }

  $("mx").addEventListener("click", function (e) {
    var b = e.target.closest("[data-cell]"); if (!b) return;
    S.exp.sel = S.exp.sel === b.dataset.cell ? null : b.dataset.cell;
    renderMatrix();
  });

  function renderCell() {
    var el = $("cellDetail");
    if (!S.exp.sel) { el.innerHTML = "select a cell to see every repeat behind it"; return; }
    var parts = S.exp.sel.split("|"), m = parts[0], k = parts[1];
    var row = S.exp.rows.filter(function (r) {
      return r.m === m && (S.exp.size == null || r.n === S.exp.size); })[0];
    var ps = row && row.p[k];
    if (!ps) { el.innerHTML = "no data"; return; }
    var mu = mean(ps), sd = sdev(ps, mu);
    el.innerHTML = "<dl>" +
      "<dt>case</dt><dd>" + esc(m) + " &rarr; " + esc(k) + "</dd>" +
      "<dt>mean</dt><dd>" + mu.toFixed(4) + " &nbsp; sd " + sd.toFixed(4) +
        " &nbsp; min " + Math.min.apply(null, ps).toFixed(3) +
        " &nbsp; max " + Math.max.apply(null, ps).toFixed(3) + "</dd>" +
      "<dt>repeats</dt><dd><div class=\"reps\">" + ps.map(function (p) {
        return '<i class="' + (p >= S.exp.thr ? "on" : "") + '">' + p.toFixed(2) + "</i>";
      }).join("") + "</div></dd></dl>";
  }

  /* ── findings ─────────────────────────────────────────────── */
  var FINDINGS_RENDERED = false;
  function renderFindings() {
    if (FINDINGS_RENDERED) return;
    FINDINGS_RENDERED = true;
    $("findingsProse").innerHTML = window.FINDINGS_HTML || "";
  }

  /* ── boot ─────────────────────────────────────────────────── */
  api("/api/config").then(function (cfg) {
    S.cfg = cfg; S.thr = cfg.threshold || 0.5; S.exp.thr = S.thr;
    $("thr").value = S.thr; $("thrVal").textContent = S.thr.toFixed(2);
    $("ethr").value = S.thr; $("ethrVal").textContent = S.thr.toFixed(2);

    setTopics(cfg.topics || []);
    S.topic = (cfg.topics || [])[0] || "alerts.infra";
    setTopics(cfg.topics || []);
    $("topic").value = S.topic;
    renderSamples();

    $("runSel").innerHTML = (cfg.runs || []).length
      ? cfg.runs.map(function (n) { return "<option>" + esc(n) + "</option>"; }).join("")
      : '<option value="">no recorded runs</option>';

    // A keyword judge is worth saying out loud; a healthy one is not
    // worth a permanent badge in the corner.
    if (!cfg.judgeLive) {
      note("Running on the keyword judge \u2014 no TYPESAFE_API_KEY, so nothing is judged semantically.");
    }

    return loadSubs();
  }).then(function () {
    renderStream();
    return api("/api/decisions");
  }).then(function (recs) {
    S.recs = (recs || []).slice().reverse(); renderStream();
    var es = new EventSource("/api/stream");
    es.onmessage = function (ev) {
      try {
        S.recs.unshift(JSON.parse(ev.data));
        if (S.recs.length > 50) S.recs.pop();
        renderStream();
      } catch (e) { /* a malformed frame must not kill the stream */ }
    };
  }).catch(function (e) { note(e.message || "could not reach the server"); });

  // A typed topic is created by using it, so commit on change/blur rather
  // than requiring a separate "create" step.
  $("topic").addEventListener("change", function (e) {
    if (e.target.value === NEW_TOPIC) {
      $("newTopicRow").hidden = false;
      $("newTopic").focus();
      $("topic").value = S.topic || "";
      return;
    }
    S.topic = e.target.value; $("newTopicRow").hidden = true;
    renderSamples(); loadSubs();
  });

  // A typed topic is created by being used, so selecting it is all there is.
  function createTopic() {
    var t = $("newTopic").value.trim();
    if (!t) return;
    S.topic = t;
    setTopics(S.topics.concat([t]));
    $("topic").value = t;
    $("newTopic").value = ""; $("newTopicRow").hidden = true;
    renderSamples(); loadSubs();
  }
  $("newTopicGo").addEventListener("click", createTopic);
  $("newTopic").addEventListener("keydown", function (e) {
    if (e.key === "Enter") { e.preventDefault(); createTopic(); }
    if (e.key === "Escape") { $("newTopicRow").hidden = true; }
  });
})();
