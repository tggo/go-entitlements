/* Simulator UI. All rules live in engine.js (ports of the Go code). */
(function () {
  'use strict';
  var E = window.Engine, d = E.d, fmt = E.fmt;
  var reduce = window.matchMedia && matchMedia('(prefers-reduced-motion: reduce)').matches;
  var USER = 'u-7e3f', PRODUCTS = ['pro.monthly'];

  var CARDS = [], ACTIONS = [], SCENARIOS = [], BY_ID = {}, ACT_BY_ID = {}, CFG = {};

  var GATES = [
    { n: 'Verify', q: 'Is it really from the store?' },
    { n: 'Read', q: 'What does it say?' },
    { n: 'Environment', q: 'A free test purchase?' },
    { n: 'Owner', q: 'Whose purchase is it?' },
    { n: 'Dedupe', q: 'Seen this ID before?' },
    { n: 'Order', q: 'Newer? Is the move allowed?' },
    { n: 'Save', q: 'Write state and ID together' }
  ];
  var RESOLVER = { 'orig-3001': 'u-7e3f' };

  var svc = new E.Service(), play = new E.Play();
  var tab = 'apple', busy = false, nowMs = d('2026-03-15');
  var log = [];

  function $(s, r) { return (r || document).querySelector(s); }
  function el(tag, cls, text) { var e = document.createElement(tag); if (cls) e.className = cls; if (text != null) e.textContent = text; return e; }
  function sleep(ms) { return new Promise(function (r) { setTimeout(r, reduce ? 0 : ms); }); }

  /* ----- rendering ----- */
  function renderTray() {
    var tray = $('#tray'); tray.textContent = '';
    if (tab === 'code') {
      ACTIONS.filter(function (a) { return !a.hidden; }).forEach(function (a) {
        var b = el('button', 'env env-act');
        b.type = 'button'; b.dataset.id = a.id;
        b.appendChild(el('span', 'env-title', a.title));
        b.appendChild(el('span', 'env-sub', a.sub));
        b.setAttribute('aria-label', 'Run ' + a.title + ': ' + a.sub);
        b.addEventListener('click', function () { act(a.id); });
        tray.appendChild(b);
      });
    } else {
      CARDS.filter(function (c) { return c.src === tab && !c.hidden; }).forEach(function (c) {
        var b = el('button', 'env' + (c.forged || c.noAuth ? ' env-odd' : ''));
        b.type = 'button'; b.draggable = true; b.dataset.id = c.id;
        b.appendChild(el('span', 'env-title', c.title));
        b.appendChild(el('span', 'env-sub', c.sub));
        b.setAttribute('aria-label', 'Send ' + c.title + ': ' + c.sub);
        b.addEventListener('click', function () { send(c.id); });
        b.addEventListener('dragstart', function (ev) { ev.dataTransfer.setData('text/plain', c.id); ev.dataTransfer.effectAllowed = 'copy'; });
        tray.appendChild(b);
      });
    }
    $('#playdown-wrap').hidden = tab !== 'google';
    $('#ackdown-wrap').hidden = tab !== 'google';
    $('#resolver-wrap').hidden = tab === 'google';
    $('#tray-help').textContent = tab === 'code'
      ? 'These are calls your own code makes. They do not go through a webhook.'
      : 'Click a message or drag it onto the pipeline.';
  }

  function renderGates(t, activeIdx) {
    var ol = $('#gates');
    Array.prototype.forEach.call(ol.children, function (li, i) {
      li.className = '';
      if (!t) return;
      var s = t.gates[i];
      if (activeIdx != null && i > activeIdx) s = 'wait';
      li.classList.add('g-' + s);
      if (i === activeIdx) li.classList.add('g-now');
    });
  }

  function renderLedger(flash) {
    var tb = $('#ledger-body'); tb.textContent = '';
    if (!svc.order.length) {
      var tr = el('tr'); var td = el('td', 'empty', 'No chains yet. Send a message.'); td.colSpan = 8; tr.appendChild(td); tb.appendChild(tr);
    }
    svc.order.forEach(function (k) {
      var e = svc.chains[k], tr = el('tr');
      if (flash && flash.indexOf(k) >= 0) tr.className = 'flash';
      tr.appendChild(el('td', '', e.source));
      tr.appendChild(el('td', 'code', e.tx));
      tr.appendChild(el('td', 'code', e.userId || 'none'));
      var td = el('td'); td.appendChild(el('span', 'pill s-' + e.state, e.state.replace('_', ' '))); tr.appendChild(td);
      tr.appendChild(el('td', '', e.state === 'expired' && !e.expiresAt ? 'none' : fmt(e.expiresAt)));
      tr.appendChild(el('td', '', e.autoRenew == null ? 'unknown' : (e.autoRenew ? 'on' : 'off')));
      tr.appendChild(el('td', '', String(e.version)));
      tr.appendChild(el('td', '', fmt(e.updatedAt)));
      tb.appendChild(tr);
    });
    renderAccess();
  }

  function renderAccess() {
    var box = $('#accesses'); box.textContent = '';
    PRODUCTS.forEach(function (p) {
      var yes = svc.isActive(USER, p, nowMs);
      var s = el('span', 'access ' + (yes ? 'yes' : 'no'));
      s.appendChild(el('span', 'code', p));
      s.appendChild(document.createTextNode(yes ? ': yes' : ': no'));
      box.appendChild(s);
    });
    $('#nowlabel').textContent = fmt(nowMs);
  }

  function renderLog() {
    var ul = $('#log'); ul.textContent = '';
    log.slice(0, 8).forEach(function (l) {
      var li = el('li'); li.appendChild(el('b', 'code', l.code ? String(l.code) : 'call'));
      li.appendChild(document.createTextNode(' ' + l.title + ' '));
      li.appendChild(el('span', 'pill p-' + l.cls, l.stamp.toLowerCase()));
      ul.appendChild(li);
    });
  }

  function showResult(title, t) {
    var r = $('#result');
    r.className = 'result r-' + t.cls;
    $('#stamp').textContent = t.stamp;
    $('#code').textContent = t.code ? 'The store receives HTTP ' + t.code : 'Returned to your code, no HTTP involved';
    $('#why').textContent = t.text;
    $('#result-title').textContent = title;
  }

  async function run(title, make) {
    if (busy) return;
    busy = true;
    $('#sim').setAttribute('aria-busy', 'true');
    var t = make();
    $('#result').className = 'result';
    $('#stamp').textContent = '';
    $('#why').textContent = 'Checking…';
    $('#code').textContent = '';
    $('#result-title').textContent = title;
    var last = t.gates.lastIndexOf('stop');
    var end = last >= 0 ? last : GATES.length - 1;
    if (t.code === 0) { end = -1; }
    for (var i = 0; i <= end; i++) { renderGates(t, i); await sleep(220); }
    renderGates(t.code === 0 ? null : t, null);
    showResult(title, t);
    log.unshift({ code: t.code, title: title, stamp: t.stamp, cls: t.cls });
    renderLog();
    renderLedger(t.changed);
    busy = false;
    $('#sim').removeAttribute('aria-busy');
  }

  function send(id) {
    var card = BY_ID[id];
    return run(card.title, function () { return E.deliver(svc, card, { wallClock: d(CFG.wallClock), play: play }); });
  }
  function act(id) {
    var a = ACT_BY_ID[id];
    return run(a.title, function () { return E.runAction(svc, a); });
  }

  function syncToggles() {
    $('#dbdown').checked = svc.dbDown;
    $('#playdown').checked = play.down;
    $('#ackdown').checked = play.ackDown;
    $('#sandbox').checked = svc.allowSandbox;
    $('#resolver').checked = !!svc.resolver;
  }

  function resetAll() {
    svc.reset(); play = new E.Play(); log = [];
    syncToggles();
    renderGates(null); renderLedger(); renderLog();
    $('#result').className = 'result'; $('#stamp').textContent = '';
    $('#why').textContent = 'Pick a message from the tray, or drop one on the pipeline.';
    $('#code').textContent = ''; $('#result-title').textContent = 'Nothing sent yet';
  }

  async function playScenario(sc) {
    if (busy) return;
    resetAll();
    svc.allowSandbox = !!sc.allowSandbox;
    svc.resolver = sc.resolver || null;
    syncToggles();
    var first = sc.steps.filter(function (s) { return s.send || s.do; })[0];
    setTab(first && first.do ? 'code' : (first ? BY_ID[first.send].src : 'apple'));
    for (var i = 0; i < sc.steps.length; i++) {
      var s = sc.steps[i];
      if (s.db) { svc.dbDown = s.db === 'down'; syncToggles(); }
      else if ('playDown' in s) { play.down = s.playDown; syncToggles(); }
      else if (s.ack) { play.ackDown = s.ack === 'down'; syncToggles(); }
      else if (s.register) { var c = BY_ID[s.register]; play.register(c, E.prepare(c)); }
      else if (s.do) { await act(s.do); await sleep(700); }
      else { await send(s.send); await sleep(700); }
    }
  }

  function setTab(name) {
    tab = name;
    ['apple', 'google', 'code'].forEach(function (n) {
      var b = $('#tab-' + n); b.setAttribute('aria-selected', n === name ? 'true' : 'false'); b.tabIndex = n === name ? 0 : -1;
    });
    renderTray();
  }

  function init() {
    fetch('scenarios.json').then(function (r) { return r.json(); }).then(function (j) {
      CFG = j; USER = j.user; PRODUCTS = j.checkProducts; CARDS = j.cards; ACTIONS = j.actions;
      CARDS.forEach(function (c) { BY_ID[c.id] = c; });
      ACTIONS.forEach(function (a) { ACT_BY_ID[a.id] = a; });
      SCENARIOS = j.scenarios.filter(function (s) { return !s.hidden; });
      build();
    }).catch(function () { $('#why').textContent = 'The simulator could not load scenarios.json.'; });
  }

  function build() {
    var gl = $('#gates');
    GATES.forEach(function (g, i) {
      var li = el('li'); li.appendChild(el('span', 'g-n', String(i + 1)));
      var w = el('span', 'g-t'); w.appendChild(el('b', '', g.n)); w.appendChild(el('span', 'g-q', g.q)); li.appendChild(w);
      gl.appendChild(li);
    });
    var sl = $('#scenarios');
    SCENARIOS.forEach(function (sc) {
      var li = el('li'); var b = el('button', 'scn', sc.name); b.type = 'button';
      b.addEventListener('click', function () { playScenario(sc); });
      li.appendChild(b); li.appendChild(el('span', 'scn-note', sc.note)); sl.appendChild(li);
    });
    var names = ['apple', 'google', 'code'];
    names.forEach(function (n, i) {
      $('#tab-' + n).addEventListener('click', function () { setTab(n); });
      $('#tab-' + n).addEventListener('keydown', function (e) {
        var k = e.key === 'ArrowRight' ? 1 : e.key === 'ArrowLeft' ? -1 : 0;
        if (k) { var o = names[(i + k + names.length) % names.length]; setTab(o); $('#tab-' + o).focus(); }
      });
    });
    $('#dbdown').addEventListener('change', function (e) { svc.dbDown = e.target.checked; });
    $('#playdown').addEventListener('change', function (e) { play.down = e.target.checked; });
    $('#ackdown').addEventListener('change', function (e) { play.ackDown = e.target.checked; });
    $('#resolver').addEventListener('change', function (e) { svc.resolver = e.target.checked ? RESOLVER : null; });
    $('#sandbox').addEventListener('change', function (e) { svc.allowSandbox = e.target.checked; renderAccess(); });
    $('#reset').addEventListener('click', function () { if (!busy) resetAll(); });
    var range = $('#now');
    range.min = 0; range.max = 120; range.value = Math.round((nowMs - d('2026-01-01')) / E.DAY);
    range.addEventListener('input', function () { nowMs = d('2026-01-01') + range.value * E.DAY; renderAccess(); });
    var drop = $('#mouth');
    drop.addEventListener('dragover', function (e) { e.preventDefault(); drop.classList.add('over'); });
    drop.addEventListener('dragleave', function () { drop.classList.remove('over'); });
    drop.addEventListener('drop', function (e) { e.preventDefault(); drop.classList.remove('over'); var id = e.dataTransfer.getData('text/plain'); if (BY_ID[id]) send(id); });
    setTab('apple'); resetAll();
  }
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', init); else init();
  window.__sim = { send: send, act: act, play: playScenario, svc: svc, cards: function () { return CARDS; }, scenarios: function () { return SCENARIOS; } };
})();
