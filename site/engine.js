/* Ports of the library rules, kept small and literal so the simulator behaves
 * like the Go code. Sources:
 *   statemachine.go  -> applyEvent (Apply, isTerminal)
 *   service.go       -> Service.process / supersede / isActive
 *   apple/mapper.go  -> appleToEvent (ToEvent, mapType, resolve, ignoredTypes)
 *   google/parser.go -> googleToEvent (fromSubscription, fromVoided, mapState)
 *   httpx/*.go       -> HTTP status per outcome
 * Times are milliseconds since epoch (UTC). */
(function (root) {
  'use strict';

  var DAY = 86400000;
  function d(s) { return Date.parse(s + 'T00:00:00Z'); }
  var MON = ['Jan','Feb','Mar','Apr','May','Jun','Jul','Aug','Sep','Oct','Nov','Dec'];
  function fmt(ms) {
    if (!ms) return 'none';
    var x = new Date(ms);
    return MON[x.getUTCMonth()] + ' ' + x.getUTCDate();
  }

  function isTerminal(s) { return s === 'refunded' || s === 'revoked'; }

  var MAX_REFUNDED = 64; // entitlements.MaxRefundedTransactions

  function withTx(list, id) {
    if (!id || list.indexOf(id) >= 0) return list;
    list = list.concat([id]);
    return list.length > MAX_REFUNDED ? list.slice(list.length - MAX_REFUNDED) : list;
  }
  function laterOf(a, b) { return b > a ? b : a; }

  /* statemachine.go: Apply. Returns {next} or {stale: reason}. */
  function applyEvent(cur, ev) {
    if (!cur) {
      return { next: {
        userId: ev.userId, productId: ev.productId, tx: ev.tx, source: ev.source,
        state: ev.state, expiresAt: ev.expiresAt, updatedAt: ev.occurredAt,
        version: 1, environment: ev.environment || '',
        refunded: isTerminal(ev.state) ? withTx([], ev.transactionId) : []
      } };
    }
    var tx = ev.transactionId || '';
    var refundedTx = !!tx && cur.refunded.indexOf(tx) >= 0;
    // A renewal (a new, unrefunded transaction that outlasts the refunded one)
    // may arrive after the REFUND of the transaction it replaces, even if it
    // was signed earlier.
    var succeeds = isTerminal(cur.state) && ev.state === 'active' && !ev.noRevive &&
      !!tx && !refundedTx && ev.expiresAt > cur.expiresAt;

    if (ev.occurredAt < cur.updatedAt && !succeeds) {
      return { stale: 'It is older than the last event applied (' + fmt(cur.updatedAt) + ').' };
    }
    if (refundedTx && !isTerminal(ev.state) && !ev.restores) {
      return { stale: 'That transaction was refunded.' };
    }
    var n = Object.assign({}, cur);
    n.refunded = cur.refunded.slice();

    if (ev.source === 'apple' && ev.state === 'refunded' && !isTerminal(cur.state) &&
        ev.expiresAt && ev.expiresAt < cur.expiresAt) {
      if (!tx) {
        return { stale: 'It refunds an older transaction (expires ' + fmt(ev.expiresAt) +
          ') but the chain already runs to ' + fmt(cur.expiresAt) + '.' };
      }
      n.refunded = withTx(n.refunded, tx);
      n.version = cur.version + 1;
      n.updatedAt = laterOf(cur.updatedAt, ev.occurredAt);
      return { next: n, note: 'refund recorded' };
    }
    if (isTerminal(cur.state) && !isTerminal(ev.state)) {
      var reactivates = !ev.noRevive && (ev.state === 'active' || ev.restores) && ev.occurredAt > cur.updatedAt;
      if (!reactivates && !succeeds) {
        return { stale: 'The chain is ' + cur.state + '. This event wants ' + ev.state +
          ', and only a new purchase, a renewal or a refund reversal may lift that.' };
      }
    }
    n.state = ev.state;
    if (isTerminal(ev.state)) n.refunded = withTx(n.refunded, tx);
    else if (ev.restores && tx) n.refunded = n.refunded.filter(function (id) { return id !== tx; });
    if (ev.expiresAt) n.expiresAt = ev.expiresAt;
    if (ev.productId) n.productId = ev.productId;
    if (ev.userId) n.userId = ev.userId;
    n.environment = ev.environment || '';
    n.updatedAt = laterOf(cur.updatedAt, ev.occurredAt);
    n.version = cur.version + 1;
    return { next: n };
  }

  function entActive(e, now) {
    if (e.state !== 'active' && e.state !== 'grace') return false;
    return !e.expiresAt || now < e.expiresAt;
  }

  /* apple/mapper.go */
  var IGNORED = { TEST:1, RENEWAL_EXTENSION:1, REFUND_DECLINED:1, PRICE_INCREASE:1, PRICE_CHANGE:1,
    CONSUMPTION_REQUEST:1, METADATA_UPDATE:1, MIGRATION:1, EXTERNAL_PURCHASE_TOKEN:1,
    RESCIND_CONSENT:1, CHILD_CONSENT_REVOKED:1 };

  function mapType(typ, sub) {
    switch (typ) {
      case 'SUBSCRIBED': case 'DID_RENEW': return { state: 'active' };
      case 'DID_FAIL_TO_RENEW': return { state: sub === 'GRACE_PERIOD' ? 'grace' : 'billing_retry' };
      case 'GRACE_PERIOD_EXPIRED': return { state: 'billing_retry' };
      case 'EXPIRED': return { state: 'expired' };
      case 'REFUND': return { state: 'refunded' };
      case 'REVOKE': return { state: 'revoked' };
      case 'DID_CHANGE_RENEWAL_STATUS': case 'DID_CHANGE_RENEWAL_PREF':
      case 'OFFER_REDEEMED': case 'RENEWAL_EXTENDED': return { ambiguous: true };
      case 'REFUND_REVERSED': return { ambiguous: true, restores: true };
    }
    return null;
  }

  function resolve(tx, status, expires, signed, restores) {
    if (tx.revocationDate && !restores) return 'refunded';
    switch (status) {
      case 1: return 'active'; case 2: return 'expired'; case 3: return 'billing_retry';
      case 4: return 'grace'; case 5: return 'revoked';
    }
    return expires > signed ? 'active' : 'expired';
  }

  /* p: {uuid,type,subtype,status,signed,expires,graceEnd,revocationDate,txType,env,userId,tx,product}
   * returns {event} or {error: 'ignored'|'unsupported'|'noexpiry', msg} */
  function appleToEvent(p) {
    if (IGNORED[p.type]) return { error: 'ignored', msg: p.type + ' carries no subscription state.' };
    if (p.type === 'ONE_TIME_CHARGE') return { error: 'ignored', msg: 'ONE_TIME_CHARGE is a non-subscription purchase, out of scope.' };
    var m = mapType(p.type, p.subtype);
    if (!m) return { error: 'unsupported', msg: 'Unknown notification type ' + p.type + '.' };
    if ((p.type === 'REFUND' || p.type === 'REVOKE') && p.txType && p.txType !== 'Auto-Renewable Subscription') {
      return { error: 'ignored', msg: p.type + ' of a ' + p.txType + ' purchase is not subscription state.' };
    }
    var state = m.state;
    if (m.ambiguous) state = resolve(p, p.status || 0, p.expires, p.signed, !!m.restores);
    else if (state === 'active' && p.revocationDate) state = 'refunded';
    var expires = p.expires;
    if (state === 'active' && !expires) return { error: 'noexpiry', msg: 'No expiry date, so access is not granted.' };
    if (state === 'grace') {
      if (!p.graceEnd) return { error: 'noexpiry', msg: 'Grace period without an end date.' };
      if (p.graceEnd > expires) expires = p.graceEnd;
    }
    return { event: {
      id: p.uuid, source: 'apple', userId: p.userId || '', tx: p.tx, transactionId: p.txn || ('t-' + p.uuid), productId: p.product,
      state: state, expiresAt: expires, occurredAt: p.signed,
      restores: !!m.restores, noRevive: !!(m.ambiguous && !m.restores),
      environment: p.env || '', supersedes: ''
    } };
  }

  /* google/parser.go. p: {kind:'sub'|'voided'|'test'|'otherPackage', messageId, eventTime, token,
   *   subState, expiry, linked, userId, product, orderMatches, testPurchase} */
  function googleMapState(s) {
    switch (s) {
      case 'ACTIVE': case 'CANCELED': return 'active';
      case 'IN_GRACE_PERIOD': return 'grace';
      case 'ON_HOLD': return 'billing_retry';
      case 'PAUSED': return 'paused';
      case 'EXPIRED': case 'PENDING_PURCHASE_CANCELED': return 'expired';
      case 'PENDING': return 'ignored';
    }
    return null;
  }

  function googleToEvent(p) {
    if (p.kind === 'otherPackage') return { error: 'ignored', msg: 'Notification is for another package name. Nothing was fetched.' };
    if (p.kind === 'test') return { error: 'ignored', msg: 'This is the Play Console test message. It carries no state.' };
    var ev = { id: p.messageId, source: 'google', occurredAt: p.eventTime, tx: p.token,
      productId: p.product, userId: p.userId || '', expiresAt: p.expiry || 0, supersedes: '',
      environment: p.testPurchase ? 'GoogleTest' : '' };
    if (p.kind === 'voided') {
      if (!p.orderMatches) return { error: 'ignored', msg: 'The voided order is not the latest order of the subscription.' };
      // fromVoided sets only ID, source, time, token and state: no user, product or expiry.
      ev.state = 'refunded'; ev.userId = ''; ev.productId = ''; ev.expiresAt = 0;
      return { event: ev };
    }
    var st = googleMapState(p.subState);
    if (st === 'ignored') return { error: 'ignored', msg: 'Still pending the first payment: nothing to grant.' };
    if (!st) return { error: 'unsupported', msg: 'Subscription state ' + p.subState + ' is not mapped.' };
    ev.state = st;
    if ((st === 'active' || st === 'grace')) {
      if (!ev.expiresAt) return { error: 'noexpiry', msg: 'No expiryTime, so access is not granted.' };
      ev.supersedes = p.linked || '';
    }
    return { event: ev };
  }

  /* service.go + a store that follows the numbered contract in interfaces.go. */
  function Service() { this.reset(); }
  Service.prototype.reset = function () {
    this.chains = {};      // "source|tx" -> entitlement
    this.processed = {};   // "source|id" -> outcome
    this.order = [];       // chain keys in creation order
    this.allowSandbox = false;
    this.dbDown = false;
  };
  Service.prototype._key = function (src, tx) { return src + '|' + tx; };

  // Store.Apply: atomic dedupe + fn + write. Returns {kind}.
  Service.prototype._storeApply = function (ev, fn) {
    if (this.dbDown) return { kind: 'error' };
    var pk = ev.source + '|' + ev.id;
    if (this.processed[pk]) return { kind: 'duplicate' };
    var ck = this._key(ev.source, ev.tx);
    var cur = this.chains[ck] || null;
    var r = fn(cur);
    if (r.stale) { this.processed[pk] = 'stale'; return { kind: 'stale', reason: r.stale, cur: cur }; }
    if (!cur) this.order.push(ck);
    this.chains[ck] = r.next;
    this.processed[pk] = 'applied';
    return { kind: 'applied', ent: r.next, before: cur, note: r.note };
  };

  // Service.Process. Returns {kind, stopGate, ...}
  Service.prototype.process = function (ev) {
    var self = this;
    if (ev.environment && ev.environment !== 'Production' && !this.allowSandbox) {
      return { kind: 'ignored', gate: 2, msg: ev.environment + ' purchases are free test purchases. The service ignores them unless built with WithAllowSandbox.' };
    }
    if (!ev.userId && ev.supersedes && ev.supersedes !== ev.tx) {
      var old = this.chains[this._key(ev.source, ev.supersedes)];
      if (old) { ev.userId = old.userId; ev.inherited = true; }
    }
    var res = this._storeApply(ev, function (cur) { return applyEvent(cur, ev); });
    var out = { kind: res.kind, ev: ev, ent: res.ent, before: res.before, reason: res.reason, note: res.note };
    if (res.kind === 'error') { out.gate = 3; return out; }
    if (ev.supersedes && ev.supersedes !== ev.tx) {
      out.superseded = this._supersede(ev);
    }
    return out;
  };

  Service.prototype._supersede = function (ev) {
    var old = { id: ev.id + '#supersede', source: ev.source, tx: ev.supersedes, state: 'expired', occurredAt: ev.occurredAt };
    var res = this._storeApply(old, function (cur) {
      if (!cur) return { next: { source: old.source, tx: old.tx, state: 'expired', updatedAt: old.occurredAt, version: 1, userId: '', productId: '', expiresAt: 0, refunded: [] } };
      if (isTerminal(cur.state) || cur.state === 'expired') return { stale: 'already ended' };
      return applyEvent(cur, old);
    });
    return res.kind === 'applied' ? (res.before ? 'closed' : 'tombstone') : 'none';
  };

  // Service.IsActive
  Service.prototype.isActive = function (userId, productId, now) {
    for (var i = 0; i < this.order.length; i++) {
      var e = this.chains[this.order[i]];
      if (e.userId !== userId || !userId) continue;
      if (e.environment && e.environment !== 'Production' && !this.allowSandbox) continue;
      if (e.productId === productId && entActive(e, now)) return true;
    }
    return false;
  };


  /* ---- everything below is shared by the page and by site/check-scenarios.js ---- */

  var GATE_COUNT = 6;
  var MAX_FUTURE_SKEW = 5 * 60 * 1000; // google.DefaultMaxFutureSkew

  // Turn a scenarios.json card (ISO dates) into the millisecond form the ports use.
  function prepare(card) {
    var p = Object.assign({}, card[card.src]);
    ['signed', 'expires', 'graceEnd', 'revocationDate', 'eventTime', 'expiry'].forEach(function (k) {
      if (typeof p[k] === 'string' && p[k]) p[k] = d(p[k]); else if (!p[k]) p[k] = 0;
    });
    if (card.src === 'apple') p.status = p.status || 0;
    return p;
  }

  /* One delivery, as httpx + the parsers + Service do it.
   * opts: {wallClock (ms), playDown (bool)}. Returns {code, stamp, cls, text, gates, changed, changed2}. */
  function deliver(svc, card, opts) {
    opts = opts || {};
    var p = prepare(card);
    var t = { gates: [], code: 200, stamp: 'APPLIED', cls: 'ok', text: '', changed: null, changed2: null };
    for (var i = 0; i < GATE_COUNT; i++) t.gates.push('skip');
    function stop(i, code, stamp, cls, text) {
      for (var k = 0; k < i; k++) t.gates[k] = 'pass';
      t.gates[i] = 'stop'; t.code = code; t.stamp = stamp; t.cls = cls; t.text = text; return t;
    }
    var res;
    if (card.src === 'apple') {
      if (card.forged) return stop(0, 401, 'FORGED', 'bad', 'The certificate chain in the header does not end at Apple Root CA - G3. The body is never read. The store gets 401 and the error is logged.');
      res = appleToEvent(p);
    } else {
      if (card.noAuth) return stop(0, 401, 'REJECTED', 'bad', 'The push carries no valid Pub/Sub token for your audience and service account, so the handler answers 401 before it reads the body.');
      if (p.kind === 'otherPackage' || p.kind === 'test') {
        res = googleToEvent(p);
      } else {
        if (opts.wallClock && p.eventTime > opts.wallClock + MAX_FUTURE_SKEW) {
          return stop(1, 400, 'REJECTED', 'bad', 'The event time is more than 5 minutes ahead of the clock. If it were stored, every genuine later event would count as older, so the push is refused as malformed (400).');
        }
        if (opts.playDown) return stop(1, 500, 'RETRY', 'bad', 'The Play API did not answer. Nothing was recorded, so the 500 makes Pub/Sub send the same push again later.');
        res = googleToEvent(p);
      }
    }
    if (res.error) {
      return stop(1, 200, res.error === 'ignored' ? 'IGNORED' : 'SKIPPED', 'mute', res.msg + ' The store gets 200 and stops retrying.');
    }
    var ev = res.event;
    var out = svc.process(ev);
    if (out.kind === 'ignored') return stop(2, 200, 'IGNORED', 'mute', out.msg + ' The store gets 200.');
    if (out.kind === 'error') return stop(3, 500, 'RETRY', 'bad', 'The database call failed. The event ID was not recorded and the state did not change, so the 500 makes the store send it again.');
    if (out.kind === 'duplicate') return stop(3, 200, 'DUPLICATE', 'dup', 'This notification ID was already processed. Nothing changes and the store gets 200, so it stops retrying.');
    if (out.kind === 'stale') return stop(4, 200, 'STALE', 'warn', out.reason + ' The ID is recorded, the state stays as it was, and the store gets 200.');
    for (var j = 0; j < GATE_COUNT; j++) t.gates[j] = 'pass';
    var e = out.ent;
    t.text = 'Chain ' + e.tx + ' is now ' + e.state + ', expires ' + fmt(e.expiresAt) + ', version ' + e.version + '.';
    if (out.note === 'refund recorded') t.text = 'The refund names an older transaction, so it is recorded against that one and the chain keeps its state (' + e.state + ', expires ' + fmt(e.expiresAt) + ', version ' + e.version + ').';
    if (ev.inherited) t.text += ' The message had no account id, so the user came from the chain it replaces.';
    if (out.superseded === 'closed') t.text += ' The old token ' + ev.supersedes + ' was closed.';
    if (out.superseded === 'tombstone') t.text += ' The old token ' + ev.supersedes + ' was never seen, so a tombstone was written.';
    t.changed = e.source + '|' + e.tx;
    if (out.superseded) t.changed2 = e.source + '|' + ev.supersedes;
    return t;
  }

  function iso(ms) { return ms ? new Date(ms).toISOString().slice(0, 10) : ''; }

  // The comparable end state: every chain plus access on each check date.
  function snapshot(svc, user, product, checkDates) {
    var chains = {};
    svc.order.forEach(function (k) {
      var e = svc.chains[k];
      chains[k] = { state: e.state, version: e.version, expires: iso(e.expiresAt), updated: iso(e.updatedAt), user: e.userId || '', refunded: (e.refunded || []).slice() };
    });
    var access = {};
    checkDates.forEach(function (c) { access[c] = svc.isActive(user, product, d(c)); });
    return { chains: chains, access: access };
  }

  root.Engine = { d: d, fmt: fmt, DAY: DAY, applyEvent: applyEvent, entActive: entActive,
    appleToEvent: appleToEvent, googleToEvent: googleToEvent, Service: Service, isTerminal: isTerminal, prepare: prepare, deliver: deliver, snapshot: snapshot, iso: iso, GATE_COUNT: GATE_COUNT };
  if (typeof module !== 'undefined' && module.exports) module.exports = root.Engine;
})(typeof window !== 'undefined' ? window : globalThis);
