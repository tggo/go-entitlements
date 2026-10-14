/* Simulator UI. All rules live in engine.js (ports of the Go code). */
(function () {
  'use strict';
  var E = window.Engine, d = E.d, fmt = E.fmt;
  var reduce = window.matchMedia && matchMedia('(prefers-reduced-motion: reduce)').matches;
  var USER = 'u-7e3f', PRODUCT = 'pro.monthly';

  var CARDS = [], SCENARIOS = [], BY_ID = {}, CFG = {};

  var GATES = [
    { n: 'Verify', q: 'Is it really from the store?' },
    { n: 'Read', q: 'What does it say?' },
    { n: 'Environment', q: 'A free test purchase?' },
    { n: 'Dedupe', q: 'Seen this ID before?' },
    { n: 'Order', q: 'Newer? Is the move allowed?' },
    { n: 'Save', q: 'Write state and ID together' }
  ];

  var svc = new E.Service();
  var tab = 'apple', busy = false, nowMs = d('2026-03-15');
  var log = [];

  function $(s, r) { return (r || document).querySelector(s); }
  function el(tag, cls, text) { var e = document.createElement(tag); if (cls) e.className = cls; if (text != null) e.textContent = text; return e; }
  function sleep(ms) { return new Promise(function (r) { setTimeout(r, reduce ? 0 : ms); }); }

  /* ----- delivery: the rules live in Engine.deliver ----- */
  function trace(card) {
    return E.deliver(svc, card, { wallClock: d(CFG.wallClock), playDown: $('#playdown').checked });
  }

  /* ----- rendering ----- */
  function renderTray() {
    var tray = $('#tray'); tray.textContent = '';
    CARDS.filter(function (c) { return c.src === tab && !c.hidden; }).forEach(function (c) {
      var b = el('button', 'env' + (c.forged || c.noAuth || c.future ? ' env-odd' : ''));
      b.type = 'button'; b.draggable = true; b.dataset.id = c.id;
      b.appendChild(el('span', 'env-title', c.title));
      b.appendChild(el('span', 'env-sub', c.sub));
      b.setAttribute('aria-label', 'Send ' + c.title + ': ' + c.sub);
      b.addEventListener('click', function () { send(c.id); });
      b.addEventListener('dragstart', function (ev) { ev.dataTransfer.setData('text/plain', c.id); ev.dataTransfer.effectAllowed = 'copy'; });
      tray.appendChild(b);
    });
    $('#playdown-wrap').hidden = tab !== 'google';
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
      var tr = el('tr'); var td = el('td', 'empty', 'No chains yet. Send a message.'); td.colSpan = 6; tr.appendChild(td); tb.appendChild(tr);
    }
    svc.order.forEach(function (k) {
      var e = svc.chains[k], tr = el('tr');
      if (flash && (k === flash.a || k === flash.b)) tr.className = 'flash';
      tr.appendChild(el('td', '', e.source));
      tr.appendChild(el('td', 'code', e.tx));
      var td = el('td'); td.appendChild(el('span', 'pill s-' + e.state, e.state.replace('_', ' '))); tr.appendChild(td);
      tr.appendChild(el('td', '', e.state === 'expired' && !e.expiresAt ? 'none' : fmt(e.expiresAt)));
      tr.appendChild(el('td', '', String(e.version)));
      tr.appendChild(el('td', '', fmt(e.updatedAt)));
      tb.appendChild(tr);
    });
    renderAccess();
  }

  function renderAccess() {
    var yes = svc.isActive(USER, PRODUCT, nowMs);
    var a = $('#access');
    a.className = 'access ' + (yes ? 'yes' : 'no');
    a.textContent = yes ? 'Yes, has access' : 'No access';
    $('#nowlabel').textContent = fmt(nowMs);
  }

  function renderLog() {
    var ul = $('#log'); ul.textContent = '';
    log.slice(0, 8).forEach(function (l) {
      var li = el('li'); li.appendChild(el('b', 'code', String(l.code)));
      li.appendChild(document.createTextNode(' ' + l.title + ' '));
      li.appendChild(el('span', 'pill p-' + l.cls, l.stamp.toLowerCase()));
      ul.appendChild(li);
    });
  }

  function showResult(card, t) {
    var r = $('#result');
    r.className = 'result r-' + t.cls;
    $('#stamp').textContent = t.stamp;
    $('#code').textContent = 'The store receives HTTP ' + t.code;
    $('#why').textContent = t.text;
    $('#result-title').textContent = card.title;
  }

  async function send(id) {
    if (busy) return;
    var card = BY_ID[id]; busy = true;
    $('#sim').setAttribute('aria-busy', 'true');
    var t = trace(card);
    $('#result').className = 'result';
    $('#stamp').textContent = '';
    $('#why').textContent = 'Checking…';
    $('#code').textContent = '';
    $('#result-title').textContent = card.title;
    var last = t.gates.lastIndexOf('stop');
    var end = last >= 0 ? last : GATES.length - 1;
    for (var i = 0; i <= end; i++) { renderGates(t, i); await sleep(260); }
    renderGates(t, null);
    showResult(card, t);
    log.unshift({ code: t.code, title: card.title, stamp: t.stamp, cls: t.cls });
    renderLog();
    renderLedger(t.changed ? { a: t.changed, b: t.changed2 } : null);
    busy = false;
    $('#sim').removeAttribute('aria-busy');
  }

  function resetAll() {
    svc.reset(); log = [];
    $('#dbdown').checked = false; $('#sandbox').checked = false;
    renderGates(null); renderLedger(); renderLog();
    $('#result').className = 'result'; $('#stamp').textContent = '';
    $('#why').textContent = 'Pick a message from the tray, or drop one on the pipeline.';
    $('#code').textContent = ''; $('#result-title').textContent = 'Nothing sent yet';
  }

  async function play(sc) {
    if (busy) return;
    resetAll();
    var seqTab = BY_ID[sc.steps.filter(function (s) { return BY_ID[s]; })[0]].src;
    setTab(seqTab);
    for (var i = 0; i < sc.steps.length; i++) {
      var s = sc.steps[i];
      if (s === 'db:down') { $('#dbdown').checked = true; svc.dbDown = true; }
      else if (s === 'db:up') { $('#dbdown').checked = false; svc.dbDown = false; }
      else { await send(s); await sleep(700); }
    }
  }

  function setTab(name) {
    tab = name;
    ['apple', 'google'].forEach(function (n) {
      var b = $('#tab-' + n); b.setAttribute('aria-selected', n === name ? 'true' : 'false'); b.tabIndex = n === name ? 0 : -1;
    });
    renderTray();
  }

  function init() {
    fetch('scenarios.json').then(function (r) { return r.json(); }).then(function (j) {
      CFG = j; USER = j.user; PRODUCT = j.product; CARDS = j.cards;
      CARDS.forEach(function (c) { BY_ID[c.id] = c; });
      SCENARIOS = j.scenarios.filter(function (s) { return !s.hidden; }).map(function (s) {
        return { name: s.name, note: s.note, steps: s.steps.map(function (x) { return x.send || (x.db === 'down' ? 'db:down' : 'db:up'); }) };
      });
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
      b.addEventListener('click', function () { play(sc); });
      li.appendChild(b); li.appendChild(el('span', 'scn-note', sc.note)); sl.appendChild(li);
    });
    ['apple', 'google'].forEach(function (n) {
      $('#tab-' + n).addEventListener('click', function () { setTab(n); });
      $('#tab-' + n).addEventListener('keydown', function (e) {
        if (e.key === 'ArrowRight' || e.key === 'ArrowLeft') { var o = n === 'apple' ? 'google' : 'apple'; setTab(o); $('#tab-' + o).focus(); }
      });
    });
    $('#dbdown').addEventListener('change', function (e) { svc.dbDown = e.target.checked; });
    $('#sandbox').addEventListener('change', function (e) { svc.allowSandbox = e.target.checked; renderAccess(); });
    $('#reset').addEventListener('click', function () { if (!busy) resetAll(); });
    var range = $('#now');
    range.min = 0; range.max = 120; range.value = Math.round((nowMs - d('2026-01-01')) / E.DAY);
    range.addEventListener('input', function () { nowMs = d('2026-01-01') + range.value * E.DAY; renderAccess(); });
    var drop = $('#mouth');
    drop.addEventListener('dragover', function (e) { e.preventDefault(); drop.classList.add('over'); });
    drop.addEventListener('dragleave', function () { drop.classList.remove('over'); });
    drop.addEventListener('drop', function (e) { e.preventDefault(); drop.classList.remove('over'); send(e.dataTransfer.getData('text/plain')); });
    setTab('apple'); resetAll();
  }
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', init); else init();
  window.__sim = { send: send, play: play, svc: svc, cards: function () { return CARDS; }, scenarios: function () { return SCENARIOS; }, trace: trace };
})();
