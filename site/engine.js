/* Ports of the library rules, kept small and literal so the simulator behaves
 * like the Go code. Sources:
 *   statemachine.go          -> applyEvent (Apply, withTx, laterOf)
 *   service.go               -> Service.process / supersede / linkUser / forgetUser / isActive
 *   apple/mapper.go          -> appleToEvent (ToEvent, mapType, resolve, autoRenew)
 *   google/parser.go         -> googleParse (ParseResult, selectItems, itemKeys, tokenKeys, retireLinked)
 *   httpx/*.go               -> deliver (status per outcome, processAll, acknowledge)
 *   apple/serverapi/*.go     -> eventFromStatus, reconcile, consumption
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
  function iso(ms) { return ms ? new Date(ms).toISOString().slice(0, 10) : ''; }

  function isTerminal(s) { return s === 'refunded' || s === 'revoked'; }

  var MAX_REFUNDED = 64;       // entitlements.MaxRefundedTransactions
  var MAX_LINKED = 4;          // google.MaxLinkedTokens
  var MAX_FUTURE_SKEW = 5 * 60 * 1000; // google.DefaultMaxFutureSkew
  var CONSUMPTION_DEADLINE = 12 * 3600 * 1000; // serverapi.ConsumptionDeadline

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
        refunded: isTerminal(ev.state) ? withTx([], ev.transactionId) : [],
        autoRenew: ev.autoRenew == null ? null : ev.autoRenew
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
    if (ev.userId && !cur.userId) n.userId = ev.userId; // the first owner stays
    if (ev.autoRenew != null) n.autoRenew = ev.autoRenew;
    n.environment = ev.environment || '';
    n.updatedAt = laterOf(cur.updatedAt, ev.occurredAt);
    n.version = cur.version + 1;
    return { next: n };
  }

  function entActive(e, now) {
    if (e.state !== 'active' && e.state !== 'grace') return false;
    return !e.expiresAt || now < e.expiresAt;
  }

  /* ---------------- Apple: apple/mapper.go ---------------- */
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

  // renewalInfo.autoRenewStatus when present, else the AUTO_RENEW_* subtype, else unknown.
  function appleAutoRenew(p) {
    if (p.autoRenew !== undefined && p.autoRenew !== null) return p.autoRenew === 1;
    if (p.type === 'DID_CHANGE_RENEWAL_STATUS' && p.subtype === 'AUTO_RENEW_ENABLED') return true;
    if (p.type === 'DID_CHANGE_RENEWAL_STATUS' && p.subtype === 'AUTO_RENEW_DISABLED') return false;
    return null;
  }

  /* p: {uuid,type,subtype,status,signed,expires,graceEnd,revocationDate,txType,env,userId,tx,txn,product,autoRenew}
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
      id: p.uuid, source: 'apple', userId: p.userId || '', tx: p.tx, transactionId: p.txn || ('t-' + p.uuid),
      productId: p.product, state: state, expiresAt: expires, occurredAt: p.signed,
      restores: !!m.restores, noRevive: !!(m.ambiguous && !m.restores),
      environment: p.env || 'Production', supersedes: '', autoRenew: appleAutoRenew(p)
    } };
  }

  /* ---------------- Google: google/parser.go ---------------- */
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
  function grants(s) { return s === 'active' || s === 'grace'; }

  // The fake Play API: one subscription per purchase token, as Google keeps them.
  function Play() { this.down = false; this.ackDown = false; this.subs = {}; this.acks = 0; }
  Play.prototype.register = function (card, p) {
    var items = (p.items && p.items.length) ? p.items : [{ product: p.product, expiry: p.expiry }];
    if (p.noItems) items = [];
    var sub = {
      state: p.subState || 'ACTIVE', linked: p.linked || '', user: p.userId || '', test: !!p.testPurchase,
      ackPending: p.ack === 'pending',
      items: items.map(function (it) {
        return { product: it.product || '', expiry: it.expiry ? (typeof it.expiry === 'string' ? d(it.expiry) : it.expiry) : 0,
          order: it.order === 'none' ? '' : (it.order || (p.orderMatches ? 'order-1' : 'order-other')),
          autoRenew: it.autoRenew !== undefined && it.autoRenew !== null ? it.autoRenew
            : (p.autoRenew !== undefined && p.autoRenew !== null ? p.autoRenew : null) };
      })
    };
    if (p.token) this.subs[p.token] = sub;
    return sub;
  };
  Play.prototype.fetch = function (token) {
    if (this.down) return { err: 'transient' };
    var s = this.subs[token];
    return s ? { sub: s } : { err: 'notfound' };
  };

  function tokenKeys(token, items) {
    var keys = [{ key: token, suffix: '' }];
    if (items.length <= 1) return keys;
    items.forEach(function (li, i) {
      var suffix = li.product ? '#' + li.product : '#' + i;
      keys.push({ key: token + suffix, suffix: suffix });
    });
    return keys;
  }

  // itemKeys: the plain token for one item, token#product for several (a repeated suffix gets #2).
  function itemKeys(token, items) {
    if (items.length === 1) return [{ key: token, suffix: '', item: items[0] }];
    var out = [], seen = {};
    items.forEach(function (li, i) {
      var suffix = li.product ? '#' + li.product : '#' + i;
      var n = seen[suffix] || 0;
      if (n > 0) suffix = suffix + '#' + (n + 1);
      seen[suffix] = (seen[suffix] || 0) + 1;
      out.push({ key: token + suffix, suffix: suffix, item: li });
    });
    return out;
  }

  function selectItems(sub, state, subscriptionId, wall) {
    var items = sub.items;
    function fill(arr) { if (arr.length === 1 && !arr[0].product) arr[0] = Object.assign({}, arr[0], { product: subscriptionId }); return arr; }
    if (!grants(state)) {
      if (!items.length) return { items: [{ product: subscriptionId, expiry: 0, autoRenew: null }] };
      return { items: fill(items.slice()) };
    }
    var multi = items.length > 1, out = [];
    items.forEach(function (li) {
      if (!li.expiry) return;                                   // pending deferred replacement
      if (multi && state === 'active' && !(li.expiry > wall)) return; // lapsed add-on
      out.push(li);
    });
    if (!out.length) return { error: 'noexpiry' };
    return { items: fill(out) };
  }

  function voidConfirmed(sub, orderId) {
    var known = false;
    for (var i = 0; i < sub.items.length; i++) {
      var o = sub.items[i].order;
      if (!o) continue;
      known = true;
      if (o === orderId) return true;
    }
    if (known) return false;
    return !(sub.state === 'ACTIVE' || sub.state === 'CANCELED' || sub.state === 'IN_GRACE_PERIOD');
  }

  function retireLinked(play, base, token, linked) {
    var evs = [], seen = {};
    seen[token] = true;
    for (var depth = 0; linked && !seen[linked] && depth < MAX_LINKED; depth++) {
      seen[linked] = true;
      var r = play.fetch(linked), keys, next = '';
      if (r.sub) { keys = tokenKeys(linked, r.sub.items); next = r.sub.linked; }
      else if (r.err === 'notfound') keys = tokenKeys(linked, []);
      else return { error: 'transient' };
      for (var i = 0; i < keys.length; i++) {
        if (depth === 0 && i === 0) continue; // the supersede link
        evs.push({ id: base.id + '#retire#' + keys[i].key, source: 'google', tx: keys[i].key, state: 'expired',
          occurredAt: base.occurredAt, userId: '', productId: '', expiresAt: 0, supersedes: '' });
      }
      linked = next;
    }
    return { events: evs };
  }

  /* ParseResult. p: card.google (dates in ms). Returns {events, ack} or {error: kind, msg}.
   * kinds: ignored (200), unsupported/noexpiry (200), malformed (400), transient (500) */
  function googleParse(p, ctx) {
    var play = ctx.play, wall = ctx.wall;
    if (p.kind === 'otherPackage') return { error: 'ignored', msg: 'Notification is for another package name. Nothing was fetched.' };
    if (ctx.wall && p.eventTime > wall + MAX_FUTURE_SKEW) {
      return { error: 'malformed', msg: 'The event time is more than 5 minutes ahead of the clock. If it were stored, every genuine later event would count as older, so the push is refused as malformed (400).' };
    }
    if (p.kind === 'test') return { error: 'ignored', msg: 'This is the Play Console test message. It carries no state.' };
    var base = { id: p.messageId, source: 'google', occurredAt: p.eventTime };
    var f = play.fetch(p.token);
    if (f.err === 'transient') return { error: 'transient', msg: 'The Play API did not answer. Nothing was recorded, so the 500 makes Pub/Sub send the same push again later.' };
    if (f.err === 'notfound') return { error: 'ignored', msg: 'Google does not know this purchase token (404). A retry cannot help, so the store gets 200.' };
    var sub = f.sub;

    if (p.kind === 'voided') {
      if (!voidConfirmed(sub, 'order-1')) return { error: 'ignored', msg: 'The voided order is not the latest order of the subscription.' };
      var evs = tokenKeys(p.token, sub.items).map(function (k, i) {
        return { id: i > 0 ? base.id + k.suffix : base.id, source: 'google', tx: k.key, state: 'refunded',
          occurredAt: base.occurredAt, userId: '', productId: '', expiresAt: 0, supersedes: '' };
      });
      return { events: evs };
    }

    var state = googleMapState(sub.state);
    if (state === 'ignored') return { error: 'ignored', msg: 'Still pending the first payment: nothing to grant.' };
    if (!state) return { error: 'unsupported', msg: 'Subscription state ' + sub.state + ' is not mapped.' };
    var sel = selectItems(sub, state, p.product || '', wall);
    if (sel.error) return { error: 'noexpiry', msg: 'No item carries an expiryTime, so access is not granted.' };
    var events = itemKeys(p.token, sel.items).map(function (k) {
      return { id: base.id + k.suffix, source: 'google', tx: k.key, state: state, productId: k.item.product,
        expiresAt: k.item.expiry, occurredAt: base.occurredAt, userId: sub.user, supersedes: '',
        environment: sub.test ? 'GoogleTest' : '', autoRenew: k.item.autoRenew };
    });
    if (grants(state) && sub.linked && sub.linked !== p.token) {
      var rl = retireLinked(play, base, p.token, sub.linked);
      if (rl.error) return { error: 'transient', msg: 'The Play API did not answer for the replaced token, so the push is retried.' };
      events[0].supersedes = sub.linked;
      events = events.concat(rl.events);
    }
    var res = { events: events };
    if (sub.ackPending && grants(state)) res.ack = { token: p.token };
    return res;
  }

  /* ---------------- Service: service.go + a store that follows interfaces.go ---------------- */
  function Service() { this.reset(); }
  Service.prototype.reset = function () {
    this.chains = {};      // "source|tx" -> entitlement
    this.processed = {};   // "source|id" -> outcome
    this.order = [];       // chain keys in creation order
    this.allowSandbox = false;
    this.dbDown = false;
    this.resolver = null;  // tx -> user, "!error" fails
    this.notifications = 0;
  };
  Service.prototype._key = function (src, tx) { return src + '|' + tx; };

  // Store.Apply: atomic dedupe + fn + write.
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

  // Service.Process. Returns {kind, gate?, ...}
  Service.prototype.process = function (ev) {
    if (ev.environment && ev.environment !== 'Production' && !this.allowSandbox) {
      return { kind: 'ignored', gate: 2, msg: ev.environment + ' purchases are free test purchases. The service ignores them unless built with WithAllowSandbox.' };
    }
    if (!ev.userId && ev.supersedes && ev.supersedes !== ev.tx) {
      var old = this.chains[this._key(ev.source, ev.supersedes)];
      if (old) { ev.userId = old.userId; ev.inherited = !!old.userId; }
    }
    if (!ev.userId && this.resolver) {
      var uid = this.resolver[ev.tx];
      if (uid === '!error') return { kind: 'error', gate: 3, resolver: true };
      if (uid) { ev.userId = uid; ev.resolved = true; }
    }
    var res = this._storeApply(ev, function (cur) { return applyEvent(cur, ev); });
    var out = { kind: res.kind, ev: ev, ent: res.ent, before: res.before, reason: res.reason, note: res.note };
    if (res.kind === 'error') { out.gate = 4; return out; }
    if (res.kind === 'applied') this.notifications++;
    if (ev.supersedes && ev.supersedes !== ev.tx) out.superseded = this._supersede(ev);
    return out;
  };

  Service.prototype._supersede = function (ev) {
    var self = this;
    var old = { id: ev.id + '#supersede', source: ev.source, tx: ev.supersedes, state: 'expired', occurredAt: ev.occurredAt };
    var res = this._storeApply(old, function (cur) {
      if (!cur) return { next: { source: old.source, tx: old.tx, state: 'expired', updatedAt: old.occurredAt, version: 1, userId: '', productId: '', expiresAt: 0, refunded: [], autoRenew: null } };
      if (isTerminal(cur.state) || cur.state === 'expired') return { stale: 'already ended' };
      return applyEvent(cur, old);
    });
    if (res.kind === 'applied') { self.notifications++; return res.before ? 'closed' : 'tombstone'; }
    return 'none';
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

  // Service.Get (for Reconcile)
  Service.prototype.get = function (src, tx) {
    if (this.dbDown) return { err: 'db' };
    var e = this.chains[this._key(src, tx)];
    return e ? { ent: e } : { err: 'notfound' };
  };

  // Service.LinkUser
  Service.prototype.linkUser = function (src, tx, user, now) {
    if (this.dbDown) return { kind: 'failed' };
    var cur = this.chains[this._key(src, tx)];
    if (!cur) return { kind: 'notfound' };
    if (cur.userId === user) return { kind: 'unchanged', ent: cur };
    if (cur.userId) return { kind: 'conflict' };
    cur.userId = user;
    cur.version = cur.version + 1;
    this.notifications++;
    return { kind: 'linked', ent: cur };
  };

  // Service.ForgetUser: entitlements go, processed event IDs stay.
  Service.prototype.forgetUser = function (user) {
    if (this.dbDown) return { kind: 'failed' };
    var self = this, n = 0;
    this.order = this.order.filter(function (k) {
      if (self.chains[k].userId === user) { delete self.chains[k]; n++; return false; }
      return true;
    });
    return { kind: 'erased', n: n };
  };

  /* ---------------- Apple Server API: apple/serverapi ---------------- */
  // EventFromStatus. a: reconcile action (dates ms).
  function eventFromStatus(a) {
    var expires = a.expires || 0, state;
    switch (a.status) {
      case 1:
        state = 'active';
        if (!expires) return { error: 'No expiry date: an active snapshot never grants open-ended access.' };
        break;
      case 2: state = 'expired'; break;
      case 3: state = 'billing_retry'; break;
      case 4:
        state = 'grace';
        if (!a.graceEnd) return { error: 'Grace period without gracePeriodExpiresDate.' };
        if (a.graceEnd > expires) expires = a.graceEnd;
        break;
      case 5: state = a.family ? 'revoked' : 'refunded'; break;
      default: return { error: 'Unknown status ' + a.status + '.' };
    }
    if (!a.signed) return { error: 'No signedDate, so the snapshot cannot be ordered.' };
    var terminal = isTerminal(state);
    var id = ['rc', a.tx, a.txn, a.status, a.expires || 0, 0, a.graceEnd || 0, a.signed].join('|');
    return { event: { id: id, source: 'apple', userId: a.user || '', tx: a.tx, transactionId: a.txn,
      productId: a.product, state: state, expiresAt: expires, occurredAt: a.signed, restores: !terminal,
      noRevive: false, environment: 'Production', supersedes: '', autoRenew: null } };
  }

  function reconcile(svc, a) {
    var r = eventFromStatus(a);
    if (r.error) return { outcome: 'failed', text: r.error };
    var ev = r.event;
    var g = svc.get('apple', ev.tx);
    if (g.err === 'db') return { outcome: 'failed', text: 'The database call failed.' };
    if (g.ent) {
      var cur = g.ent;
      if (cur.state === ev.state && cur.expiresAt === ev.expiresAt && (!ev.productId || cur.productId === ev.productId) &&
          (cur.environment || '') === ev.environment) {
        return { outcome: 'unchanged', ent: cur, text: 'The stored state already matches what Apple says, so nothing was written.' };
      }
    }
    var out = svc.process(ev);
    switch (out.kind) {
      case 'applied': return { outcome: 'applied', ent: out.ent, text: 'Applied the snapshot.' };
      case 'duplicate': return { outcome: 'duplicate', text: 'This exact snapshot was applied before.' };
      case 'stale': return { outcome: 'stale', text: out.reason + ' The snapshot is older than what is stored.' };
      case 'ignored': return { outcome: 'ignored', text: out.msg };
    }
    return { outcome: 'failed', text: 'The database call failed.' };
  }

  // NewConsumptionHandler + RespondToConsumptionRequest. p: card.apple (ms), wall: handler clock.
  function consumption(p, wall) {
    var deadline = p.signed + CONSUMPTION_DEADLINE;
    switch (p.consumption) {
      case 'yes': return { code: 200, stamp: 'SENT', text: 'The customer agreed, so the answer (delivered, sample provided, refund preference) was sent to Apple. The store gets 200.' };
      case 'no': return { code: 200, stamp: 'NOT SENT', text: 'The customer did not agree to share usage data, so nothing is sent. The store gets 200.' };
    }
    if (!(wall < deadline)) return { code: 200, stamp: 'TOO LATE', text: 'Your lookup failed, but the 12 hours to answer have passed. A retry could not help, so the store gets 200 and stops.' };
    return { code: 500, stamp: 'RETRY', text: 'Your lookup failed inside the 12 hours. The store gets 500, so Apple sends the request again.' };
  }

  /* ---------------- shared by the page and by site/check-scenarios.js ---------------- */
  var GATE_COUNT = 7;

  // Turn a scenarios.json card (ISO dates) into the millisecond form the ports use.
  function prepare(card) {
    var p = Object.assign({}, card[card.src]);
    ['signed', 'expires', 'graceEnd', 'revocationDate', 'eventTime', 'expiry'].forEach(function (k) {
      if (typeof p[k] === 'string' && p[k]) p[k] = d(p[k]); else if (!p[k]) p[k] = 0;
    });
    if (card.src === 'apple') p.status = p.status || 0;
    return p;
  }

  function newTrace() {
    var t = { gates: [], code: 200, stamp: 'APPLIED', cls: 'ok', text: '', detail: '', acked: false, changes: 0, changed: [] };
    for (var i = 0; i < GATE_COUNT; i++) t.gates.push('skip');
    return t;
  }
  function stopAt(t, i, code, stamp, cls, text) {
    for (var k = 0; k < i; k++) t.gates[k] = 'pass';
    t.gates[i] = 'stop'; t.code = code; t.stamp = stamp; t.cls = cls; t.text = text; return t;
  }
  function allPass(t) { for (var j = 0; j < GATE_COUNT; j++) t.gates[j] = 'pass'; }

  /* One delivery, as httpx + the parsers + Service do it.
   * opts: {wallClock (ms), play (Play)}. Returns the trace. */
  function deliver(svc, card, opts) {
    opts = opts || {};
    var p = prepare(card), play = opts.play || new Play(), wall = opts.wallClock || 0;
    var t = newTrace(), notesBefore = svc.notifications;
    var events, ack = null;

    if (card.src === 'apple') {
      if (card.forged) return stopAt(t, 0, 401, 'FORGED', 'bad', 'The certificate chain in the header does not end at Apple Root CA - G3. The body is never read. The store gets 401 and the error is logged.');
      if (p.type === 'CONSUMPTION_REQUEST') {
        var c = consumption(p, wall);
        t.code = c.code; t.stamp = c.stamp; t.text = c.text;
        t.cls = c.stamp === 'SENT' ? 'ok' : (c.stamp === 'RETRY' ? 'bad' : 'mute');
        t.gates[0] = 'pass'; t.gates[1] = c.stamp === 'SENT' ? 'pass' : 'stop';
        return t;
      }
      var res = appleToEvent(p);
      if (res.error) return stopAt(t, 1, 200, res.error === 'ignored' ? 'IGNORED' : 'SKIPPED', 'mute', res.msg + ' The store gets 200 and stops retrying.');
      events = [res.event];
    } else {
      if (card.noAuth) return stopAt(t, 0, 401, 'REJECTED', 'bad', 'The push carries no valid Pub/Sub token for your audience and service account, so the handler answers 401 before it reads the body.');
      if (card.src === 'google' && p.kind !== 'otherPackage' && p.kind !== 'test') play.register(card, p);
      var g = googleParse(p, { play: play, wall: wall });
      if (g.error) {
        var map = { ignored: [200, 'IGNORED', 'mute'], unsupported: [200, 'SKIPPED', 'mute'], noexpiry: [200, 'SKIPPED', 'mute'],
          malformed: [400, 'REJECTED', 'bad'], transient: [500, 'RETRY', 'bad'] }[g.error];
        var tail = map[0] === 200 ? ' The store gets 200 and stops retrying.' : '';
        return stopAt(t, 1, map[0], map[1], map[2], g.msg + tail);
      }
      events = g.events; ack = g.ack;
    }

    // processAll: every event in order; any retryable failure makes the whole push 500.
    var outs = [], status = 200, allStored = true, failGate = -1, failText = '';
    events.forEach(function (ev) {
      var out = svc.process(ev);
      outs.push(out);
      var stored = true;
      if (out.kind === 'error') { status = 500; stored = false; if (failGate < 0) { failGate = out.gate; failText = out.resolver ? 'Your user resolver failed. The event was not stored, so the 500 makes the store send it again.' : 'The database call failed. The event ID was not recorded and the state did not change, so the 500 makes the store send it again.'; } }
      else if (out.kind === 'ignored') stored = ev.environment && ev.environment !== 'Production';
      allStored = allStored && stored;
    });
    var kinds = outs.map(function (o) { return o.kind === 'error' ? 'error' : o.kind; });
    if (events.length > 1) t.detail = kinds.join(',');

    var ackFailed = false;
    if (status === 200 && allStored && ack && events.length > 0) {
      if (play.ackDown) { ackFailed = true; status = 500; }
      else { play.acks++; t.acked = true; }
    }
    t.changes = svc.notifications - notesBefore;

    var first = outs[0];
    if (kinds.indexOf('error') >= 0) return stopAt(t, failGate, 500, 'RETRY', 'bad', failText);
    if (ackFailed) { allPass(t); t.code = 500; t.stamp = 'RETRY'; t.cls = 'bad'; t.text = 'The purchase is stored, but Google refused the acknowledgement. The 500 makes Pub/Sub send the push again; the duplicate is skipped and the acknowledgement is tried once more.'; t.changed = changedKeys(outs); return t; }
    if (first.kind === 'ignored') return stopAt(t, first.gate, 200, 'IGNORED', 'mute', first.msg + ' The store gets 200.');
    if (first.kind === 'duplicate') {
      t.text = 'This notification ID was already processed. Nothing changes and the store gets 200, so it stops retrying.';
      if (t.acked) t.text += ' The purchase was still pending, so it is acknowledged again.';
      if (t.acked) { allPass(t); t.gates[4] = 'stop'; t.code = 200; t.stamp = 'DUPLICATE'; t.cls = 'dup'; return t; }
      return stopAt(t, 4, 200, 'DUPLICATE', 'dup', t.text);
    }
    if (first.kind === 'stale') return stopAt(t, 5, 200, 'STALE', 'warn', first.reason + ' The ID is recorded, the state stays as it was, and the store gets 200.');
    allPass(t);
    var e = first.ent, ev0 = events[0];
    t.text = 'Chain ' + e.tx + ' is now ' + e.state + ', expires ' + fmt(e.expiresAt) + ', version ' + e.version + '.';
    if (first.note === 'refund recorded') t.text = 'The refund names an older transaction, so it is recorded against that one and the chain keeps its state (' + e.state + ', expires ' + fmt(e.expiresAt) + ', version ' + e.version + ').';
    if (ev0.inherited) t.text += ' The message had no account id, so the user came from the chain it replaces.';
    if (ev0.resolved) t.text += ' The message had no account id, so your user resolver supplied ' + e.userId + '.';
    if (!ev0.userId) t.text += ' It has no user, so nobody has access yet: link it with LinkUser.';
    else if (ev0.userId !== e.userId) t.text += ' The message names ' + ev0.userId + ', but the chain belongs to ' + e.userId + ' and keeps that owner.';
    if (events.length > 1) t.text += ' This push became ' + events.length + ' events (' + kinds.join(', ') + ').';
    if (first.superseded === 'closed') t.text += ' The old token ' + ev0.supersedes + ' was closed.';
    if (first.superseded === 'tombstone') t.text += ' The old token ' + ev0.supersedes + ' was never seen, so a tombstone was written.';
    if (t.acked) t.text += ' The purchase was acknowledged to Google.';
    t.changed = changedKeys(outs);
    return t;
  }

  function changedKeys(outs) {
    var keys = [];
    outs.forEach(function (o) {
      if (o.kind === 'applied' && o.ent) keys.push(o.ent.source + '|' + o.ent.tx);
      if (o.superseded && o.ev) keys.push(o.ev.source + '|' + o.ev.supersedes);
    });
    return keys;
  }

  /* An action: something your code does. a: scenarios.json action. */
  function runAction(svc, a) {
    var t = newTrace(), before = svc.notifications;
    t.code = 0;
    function done(stamp, cls, text) { t.stamp = stamp; t.cls = cls; t.text = text; t.changes = svc.notifications - before; return t; }
    if (a.kind === 'link') {
      var r = svc.linkUser(a.source, a.tx, a.user);
      if (r.ent) t.changed = [a.source + '|' + a.tx];
      switch (r.kind) {
        case 'linked': return done('LINKED', 'ok', 'Chain ' + a.tx + ' now belongs to ' + a.user + '.');
        case 'unchanged': return done('ALREADY LINKED', 'mute', 'The chain already belongs to ' + a.user + '. Nothing changed.');
        case 'conflict': return done('CONFLICT', 'bad', 'The chain belongs to someone else. An owner is never reassigned (ErrUserConflict).');
        case 'notfound': return done('NOT FOUND', 'warn', 'No notification for ' + a.tx + ' has arrived yet (ErrNotFound). Retry after it does.');
      }
      return done('FAILED', 'bad', 'The database call failed.');
    }
    if (a.kind === 'forget') {
      var f = svc.forgetUser(a.user);
      if (f.kind === 'failed') return done('FAILED', 'bad', 'The database call failed.');
      t.detail = String(f.n);
      return done('ERASED', 'ok', f.n + ' entitlement' + (f.n === 1 ? '' : 's') + ' deleted. Processed event IDs are kept, so a redelivered webhook is still a duplicate.');
    }
    if (a.kind === 'reconcile') {
      var pa = { tx: a.tx, txn: a.txn, status: a.status, family: !!a.family, user: a.user, product: a.product,
        signed: a.signed ? d(a.signed) : 0, expires: a.expires ? d(a.expires) : 0, graceEnd: a.graceEnd ? d(a.graceEnd) : 0 };
      var rc = reconcile(svc, pa);
      t.detail = rc.outcome;
      if (rc.outcome === 'applied') t.changed = ['apple|' + a.tx];
      return done(rc.outcome.toUpperCase(), { applied: 'ok', unchanged: 'mute', duplicate: 'dup', stale: 'warn', ignored: 'mute', failed: 'bad' }[rc.outcome], rc.text);
    }
    return done('FAILED', 'bad', 'Unknown action.');
  }

  // The comparable end state: every chain plus access on each check date and product.
  function snapshot(svc, user, products, checkDates) {
    var chains = {};
    svc.order.forEach(function (k) {
      var e = svc.chains[k];
      chains[k] = { state: e.state, version: e.version, expires: iso(e.expiresAt), updated: iso(e.updatedAt), user: e.userId || '',
        refunded: (e.refunded || []).slice(), autoRenew: e.autoRenew == null ? null : e.autoRenew };
    });
    var access = {};
    products.forEach(function (p, i) {
      checkDates.forEach(function (c) { access[i === 0 ? c : c + ' ' + p] = svc.isActive(user, p, d(c)); });
    });
    return { chains: chains, access: access };
  }

  // Expected shape of one step, from a trace plus the end-state snapshot.
  function outcome(t, snap) {
    return { code: t.code, stamp: t.stamp, detail: t.detail, acked: t.acked, changes: t.changes, chains: snap.chains, access: snap.access };
  }

  root.Engine = { d: d, fmt: fmt, iso: iso, DAY: DAY, applyEvent: applyEvent, entActive: entActive,
    appleToEvent: appleToEvent, Service: Service, Play: Play, isTerminal: isTerminal, prepare: prepare,
    deliver: deliver, runAction: runAction, snapshot: snapshot, outcome: outcome, GATE_COUNT: GATE_COUNT };
  if (typeof module !== 'undefined' && module.exports) module.exports = root.Engine;
})(typeof window !== 'undefined' ? window : globalThis);
