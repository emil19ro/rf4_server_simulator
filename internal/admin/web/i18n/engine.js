// engine.js — shared Chinese → English UI translation layer.
//
// Used by the launcher control panel and the admin console (see
// specs/changes/ui-english-i18n). The pages keep Chinese as their source text;
// this engine swaps visible text, attributes and later DOM updates to English
// from a per-page dictionary, and adds an EN / 中文 toggle. Strings missing
// from the dictionary simply stay Chinese, so upstream UI changes never break.
//
//   RF4I18N.start({ dict, patterns, skip })
//     dict      { "中文原文": "English" }
//     patterns  [[/regex/g, "replacement $1"], ...] for strings with values inside
//     skip      CSS selector whose content is never translated (raw logs, user data)
//
// Default language: window.__LANG__ / window.RF4_LANG (set by the server page
// shell), else localStorage "rf4.lang", else window.RF4_DEFAULT_LANG
// (injected by the server from game.language), else Chinese.
(function () {
  "use strict";
  if (window.RF4I18N) return;
  var KEY = "rf4.lang";
  var CJK = /[　-〿一-鿿＀-￯]/;
  var CJK_CHAR = /^[一-鿿]$/;
  var ATTRS = ["placeholder", "title", "aria-label", "alt", "data-tip"];
  var SKIP_TAGS = { SCRIPT: 1, STYLE: 1, TEXTAREA: 1, CODE: 1, PRE: 1 };

  function currentLang() {
    // Language decided by the server (?lang= / cookie / game.language) wins:
    // upstream's own i18n already rendered the page in that language.
    var srv = window.__LANG__ || window.RF4_LANG;
    if (srv === "en" || srv === "zh") return srv;
    try { var v = localStorage.getItem(KEY); if (v === "en" || v === "zh") return v; } catch (e) {}
    return window.RF4_DEFAULT_LANG === "en" ? "en" : "zh";
  }

  function escapeRe(s) { return s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"); }

  function makeTranslator(dict, patterns, post) {
    post = post || [];
    var has = Object.prototype.hasOwnProperty;
    var multi = [], single = [];
    Object.keys(dict).forEach(function (k) {
      (CJK_CHAR.test(k) ? single : multi).push(k);
    });
    multi.sort(function (a, b) { return b.length - a.length; });
    // One alternation over all phrases (longest first) instead of a loop per
    // key: the admin dictionary carries thousands of database names.
    var multiRe = multi.length ? new RegExp(multi.map(escapeRe).join("|"), "g") : null;
    // A single Chinese character (unit words like 件/条/级) is only replaced next
    // to a digit, space or edge, so names written in Chinese are not mangled.
    var singleRe = single.length
      ? new RegExp("(^|[\\d\\s.,:;()/%×+\\-])(" + single.map(escapeRe).join("|") + ")(?=$|[\\d\\s.,:;()/%×+\\-])", "g")
      : null;
    return function tr(text) {
      if (!text || !CJK.test(text)) return text;
      var trimmed = text.trim();
      if (has.call(dict, trimmed)) return text.replace(trimmed, dict[trimmed]);
      var out = text;
      for (var i = 0; i < patterns.length; i++) out = out.replace(patterns[i][0], patterns[i][1]);
      if (multiRe && CJK.test(out)) {
        out = out.replace(multiRe, function (m) { return dict[m]; });
      }
      if (singleRe && CJK.test(out)) {
        out = out.replace(singleRe, function (_, pre, ch) { return pre + dict[ch]; });
      }
      // post: rules for leftovers that only make sense next to already
      // translated English (e.g. a category suffix glued to an English name).
      for (var q = 0; q < post.length && CJK.test(out); q++) out = out.replace(post[q][0], post[q][1]);
      return out;
    };
  }

  function addToggle(lang) {
    // Upstream's ?lang= switcher replaces our button when it is on the page.
    if (document.getElementById("rf4LangToggle") || document.getElementById("lang-switch")) return;
    var b = document.createElement("button");
    b.type = "button";
    b.id = "rf4LangToggle";
    b.textContent = lang === "en" ? "中文" : "EN";
    b.title = lang === "en" ? "切换到中文" : "Switch to English";
    b.setAttribute("data-no-i18n", "");
    b.setAttribute("style", "position:fixed;right:14px;bottom:14px;z-index:2147483000;padding:6px 12px;border-radius:999px;" +
      "border:1px solid rgba(127,127,127,.45);background:rgba(20,24,30,.85);color:#fff;font:600 12px/1.2 system-ui,sans-serif;cursor:pointer");
    b.addEventListener("click", function () {
      try { localStorage.setItem(KEY, lang === "en" ? "zh" : "en"); } catch (e) {}
      location.reload();
    });
    document.body.appendChild(b);
  }

  function start(opts) {
    var lang = currentLang();
    var tr = makeTranslator(opts.dict || {}, opts.patterns || [], opts.post || []);
    var skipSel = "[data-no-i18n]" + (opts.skip ? ", " + opts.skip : "");
    window.RF4I18N.tr = tr;

    function untranslatable(el) {
      return !el || SKIP_TAGS[el.nodeName] || (el.closest && el.closest(skipSel));
    }
    function translateNode(node) {
      if (node.nodeType === 3) {
        if (untranslatable(node.parentNode)) return;
        var t = tr(node.nodeValue);
        if (t !== node.nodeValue) node.nodeValue = t;
        return;
      }
      if (node.nodeType !== 1 || untranslatable(node)) return;
      // Only write when the text actually changes: every write is a DOM
      // mutation, and rewriting an untranslatable value would loop forever.
      for (var a = 0; a < ATTRS.length; a++) {
        var v = node.getAttribute(ATTRS[a]);
        if (v && CJK.test(v)) {
          var tv = tr(v);
          if (tv !== v) node.setAttribute(ATTRS[a], tv);
        }
      }
      if ((node.nodeName === "INPUT" && (node.type === "button" || node.type === "submit")) && CJK.test(node.value)) {
        var nv = tr(node.value);
        if (nv !== node.value) node.value = nv;
      }
      for (var c = node.firstChild; c; c = c.nextSibling) translateNode(c);
      if (opts.afterNode) opts.afterNode(node);
    }

    function run() {
      addToggle(lang);
      if (lang !== "en") return;
      document.documentElement.lang = "en";
      document.title = tr(document.title);
      translateNode(document.body);
      var nativeConfirm = window.confirm, nativeAlert = window.alert, nativePrompt = window.prompt;
      window.confirm = function (m) { return nativeConfirm.call(window, tr(String(m))); };
      window.alert = function (m) { return nativeAlert.call(window, tr(String(m))); };
      window.prompt = function (m, d) { return nativePrompt.call(window, tr(String(m)), d); };
      new MutationObserver(function (records) {
        for (var i = 0; i < records.length; i++) {
          var r = records[i];
          if (r.type === "childList") {
            for (var k = 0; k < r.addedNodes.length; k++) translateNode(r.addedNodes[k]);
          } else {
            translateNode(r.target);
          }
        }
        if (CJK.test(document.title)) document.title = tr(document.title);
      }).observe(document.body, { childList: true, subtree: true, characterData: true, attributes: true, attributeFilter: ATTRS });
    }

    if (document.body) run();
    else document.addEventListener("DOMContentLoaded", run);
  }

  window.RF4I18N = { start: start, lang: currentLang, makeTranslator: makeTranslator };
})();
