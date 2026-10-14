/* Simulator UI. All rules live in engine.js (ports of the Go code). */
(function () {
  'use strict';
  var E = window.Engine, d = E.d, fmt = E.fmt;
  var reduce = window.matchMedia && matchMedia('(prefers-reduced-motion: reduce)').matches;
  var USER = 'u-7e3f', PRODUCT = 'pro.monthly';

  function ap(o) {
    return Object.assign({ type: 'DID_RENEW', tx: 'orig-1001', product: PRODUCT, userId: USER, env: '', status: 0 }, o);
  }
  function gp(o) {
    return Object.assign({ kind: 'sub', product: PRODUCT, userId: USER, subState: 'ACTIVE' }, o);
  }

  var CARDS = [
    { id: 'a1', src: 'apple', title: 'SUBSCRIBED', sub: 'signed Jan 1, runs to Feb 1', p: ap({ uuid: 'n-01', type: 'SUBSCRIBED', signed: d('2026-01-01'), expires: d('2026-02-01') }) },
    { id: 'a2', src: 'apple', title: 'DID_RENEW', sub: 'signed Feb 1, runs to Mar 1', p: ap({ uuid: 'n-02', signed: d('2026-02-01'), expires: d('2026-03-01') }) },
    { id: 'a3', src: 'apple', title: 'DID_RENEW', sub: 'signed Mar 1, runs to Apr 1', p: ap({ uuid: 'n-03', signed: d('2026-03-01'), expires: d('2026-04-01') }) },
    { id: 'a4', src: 'apple', title: 'DID_FAIL_TO_RENEW', sub: 'grace period, signed Apr 1, until Apr 8', p: ap({ uuid: 'n-04', type: 'DID_FAIL_TO_RENEW', subtype: 'GRACE_PERIOD', signed: d('2026-04-01'), expires: d('2026-04-01'), graceEnd: d('2026-04-08') }) },
    { id: 'a5', src: 'apple', title: 'EXPIRED', sub: 'signed Apr 8', p: ap({ uuid: 'n-05', type: 'EXPIRED', signed: d('2026-04-08'), expires: d('2026-04-01') }) },
    { id: 'a6', src: 'apple', title: 'REFUND', sub: 'of the Mar 1 renewal, signed Mar 10', p: ap({ uuid: 'n-06', type: 'REFUND', signed: d('2026-03-10'), expires: d('2026-04-01'), txType: 'Auto-Renewable Subscription' }) },
    { id: 'a7', src: 'apple', title: 'REFUND', sub: 'of the older Feb 1 renewal, signed Mar 10', p: ap({ uuid: 'n-07', type: 'REFUND', signed: d('2026-03-10'), expires: d('2026-03-01') }) },
    { id: 'a8', src: 'apple', title: 'DID_CHANGE_RENEWAL_STATUS', sub: 'auto-renew switched off, signed Mar 12', p: ap({ uuid: 'n-08', type: 'DID_CHANGE_RENEWAL_STATUS', status: 1, signed: d('2026-03-12'), expires: d('2026-04-01') }) },
    { id: 'a9', src: 'apple', title: 'REFUND_REVERSED', sub: 'Apple took the refund back, signed Mar 14', p: ap({ uuid: 'n-09', type: 'REFUND_REVERSED', status: 1, signed: d('2026-03-14'), expires: d('2026-04-01') }) },
    { id: 'a10', src: 'apple', title: 'DID_RENEW (Sandbox)', sub: 'a TestFlight tester, signed Mar 5', p: ap({ uuid: 'n-10', env: 'Sandbox', signed: d('2026-03-05'), expires: d('2026-04-05') }) },
    { id: 'a11', src: 'apple', title: 'TEST', sub: 'sent from App Store Connect', p: ap({ uuid: 'n-11', type: 'TEST', signed: d('2026-03-05'), expires: 0 }) },
    { id: 'a12', src: 'apple', title: 'REFUND (forged)', sub: 'signed by a certificate chain Apple never issued', forged: true, p: ap({ uuid: 'n-12', type: 'REFUND', signed: d('2026-03-20'), expires: d('2026-04-01') }) },

    { id: 'g1', src: 'google', title: 'Purchased (token A)', sub: 'event Jan 1, Play says ACTIVE to Feb 1', p: gp({ messageId: 'm-101', token: 'tokA', eventTime: d('2026-01-01'), expiry: d('2026-02-01') }) },
    { id: 'g2', src: 'google', title: 'Renewed (token A)', sub: 'event Feb 1, Play says ACTIVE to Mar 1', p: gp({ messageId: 'm-102', token: 'tokA', eventTime: d('2026-02-01'), expiry: d('2026-03-01') }) },
    { id: 'g3', src: 'google', title: 'Upgrade (token B)', sub: 'event Feb 20, new token linked to token A, no account id', p: gp({ messageId: 'm-103', token: 'tokB', linked: 'tokA', userId: '', eventTime: d('2026-02-20'), expiry: d('2026-03-20') }) },
    { id: 'g4', src: 'google', title: 'Late renewal (token A)', sub: 'event Feb 15, delivered after the upgrade', p: gp({ messageId: 'm-104', token: 'tokA', eventTime: d('2026-02-15'), expiry: d('2026-03-15') }) },
    { id: 'g5', src: 'google', title: 'On hold (token B)', sub: 'event Mar 25, payment failed', p: gp({ messageId: 'm-105', token: 'tokB', subState: 'ON_HOLD', eventTime: d('2026-03-25'), expiry: d('2026-03-20') }) },
    { id: 'g6', src: 'google', title: 'Voided purchase (token B)', sub: 'event Mar 28, refund of the latest order', p: gp({ messageId: 'm-106', token: 'tokB', kind: 'voided', orderMatches: true, eventTime: d('2026-03-28') }) },
    { id: 'g7', src: 'google', title: 'License-tester purchase', sub: 'event Mar 5, free test purchase', p: gp({ messageId: 'm-108', token: 'tokT', testPurchase: true, eventTime: d('2026-03-05'), expiry: d('2026-04-05') }) },
    { id: 'g8', src: 'google', title: 'Test message', sub: 'sent from the Play Console', p: gp({ messageId: 'm-107', kind: 'test', eventTime: d('2026-03-05') }) },
    { id: 'g9', src: 'google', title: 'Another app’s package', sub: 'package name does not match yours', p: gp({ messageId: 'm-109', kind: 'otherPackage', eventTime: d('2026-03-05') }) },
    { id: 'g10', src: 'google', title: 'Push without a token', sub: 'no Pub/Sub OIDC token on the request', noAuth: true, p: gp({ messageId: 'm-110', token: 'tokA', eventTime: d('2026-03-20'), subState: 'EXPIRED' }) },
    { id: 'g11', src: 'google', title: 'Event dated next year', sub: 'a forged clock that would block every later event', future: true, p: gp({ messageId: 'm-111', token: 'tokA', eventTime: d('2027-01-01'), expiry: d('2027-02-01') }) }
  ];
  var BY_ID = {}; CARDS.forEach(function (c) { BY_ID[c.id] = c; });

  var SCENARIOS = [
    { name: 'Normal life', note: 'Subscribe, renew, renew. Each message moves the version up by one.', steps: ['a1', 'a2', 'a3'] },
    { name: 'The store sends it twice', note: 'The second DID_RENEW has the same notification ID, so nothing changes.', steps: ['a1', 'a2', 'a2'] },
    { name: 'Messages out of order', note: 'The March renewal arrives before the February one. February is older than what is stored.', steps: ['a1', 'a3', 'a2'] },
    { name: 'Refund, then a bookkeeping message', note: 'After a refund, switching auto-renew off must not give access back. Only a reversal does.', steps: ['a1', 'a3', 'a6', 'a8', 'a9'] },
    { name: 'A forged refund', note: 'The forged message is rejected before it is even read. The user keeps access.', steps: ['a1', 'a3', 'a12'] },
    { name: 'Refund of an old renewal', note: 'Refunding the February renewal does not end a chain that already runs to April.', steps: ['a1', 'a3', 'a7'] },
    { name: 'Google upgrade, then a late message', note: 'Token B closes token A. A late token A event is stale.', steps: ['g1', 'g3', 'g4'] },
    { name: 'The database blips', note: 'While the database is down the answer is 500 and nothing is recorded, so the retry works.', steps: ['db:down', 'a1', 'db:up', 'a1'] }
  ];

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

  /* ----- delivery: returns a trace ----- */
  function trace(card) {
    var t = { gates: GATES.map(function () { return 'skip'; }), code: 200, stamp: 'APPLIED', cls: 'ok', text: '', changed: null };
    function stop(i, code, stamp, cls, text) {
      for (var k = 0; k < i; k++) t.gates[k] = 'pass';
      t.gates[i] = 'stop'; t.code = code; t.stamp = stamp; t.cls = cls; t.text = text; return t;
    }
    var res;
    if (card.src === 'apple') {
      if (card.forged) return stop(0, 401, 'FORGED', 'bad', 'The certificate chain in the header does not end at Apple Root CA - G3. The body is never read. The store gets 401 and the error is logged.');
      res = E.appleToEvent(card.p);
    } else {
      if (card.noAuth) return stop(0, 401, 'REJECTED', 'bad', 'The push carries no valid Pub/Sub token for your audience and service account, so the handler answers 401 before it reads the body.');
      if (card.kind === 'x') res = null;
      if (card.p.kind === 'otherPackage' || card.p.kind === 'test') {
        res = E.googleToEvent(card.p);
      } else {
        if (card.future) return stop(1, 400, 'REJECTED', 'bad', 'The event time is more than 5 minutes ahead of the clock. If it were stored, every genuine later event would count as older, so the push is refused as malformed (400).');
        if (document.getElementById('playdown').checked) return stop(1, 500, 'RETRY', 'bad', 'The Play API did not answer. Nothing was recorded, so the 500 makes Pub/Sub send the same push again later.');
        res = E.googleToEvent(card.p);
      }
    }
    if (res.error) {
      var label = res.error === 'ignored' ? 'IGNORED' : 'SKIPPED';
      return stop(1, 200, label, 'mute', res.msg + ' The store gets 200 and stops retrying.');
    }
    var ev = res.event;
    var out = svc.process(ev);
    if (out.kind === 'ignored') return stop(2, 200, 'IGNORED', 'mute', out.msg + ' The store gets 200.');
    if (out.kind === 'error') return stop(3, 500, 'RETRY', 'bad', 'The database call failed. The event ID was not recorded and the state did not change, so the 500 makes the store send it again.');
    if (out.kind === 'duplicate') return stop(3, 200, 'DUPLICATE', 'dup', 'This notification ID was already processed. Nothing changes and the store gets 200, so it stops retrying.');
    if (out.kind === 'stale') return stop(4, 200, 'STALE', 'warn', out.reason + ' The ID is recorded, the state stays as it was, and the store gets 200.');
    for (var i = 0; i < 6; i++) t.gates[i] = 'pass';
    var e = out.ent;
    t.text = 'Chain ' + e.tx + ' is now ' + e.state + ', expires ' + fmt(e.expiresAt) + ', version ' + e.version + '.';
    if (out.ev.inherited) t.text += ' The message had no account id, so the user came from the chain it replaces.';
    if (out.superseded === 'closed') t.text += ' The old token ' + ev.supersedes + ' was closed.';
    if (out.superseded === 'tombstone') t.text += ' The old token ' + ev.supersedes + ' was never seen, so a tombstone was written.';
    t.changed = e.source + '|' + e.tx;
    if (out.superseded) t.changed2 = e.source + '|' + ev.supersedes;
    return t;
  }

  /* ----- rendering ----- */
  function renderTray() {
    var tray = $('#tray'); tray.textContent = '';
    CARDS.filter(function (c) { return c.src === tab; }).forEach(function (c) {
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
  window.__sim = { send: send, play: play, svc: svc, CARDS: CARDS, SCENARIOS: SCENARIOS, trace: trace };
})();
