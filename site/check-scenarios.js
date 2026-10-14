#!/usr/bin/env node
/* Runs engine.js (the simulator's JS port) over site/scenarios.json and checks
 * every step against the "expect" blocks that the Go test wrote from the real
 * library. A mismatch means the port drifted from the Go code. */
'use strict';
const fs = require('fs');
const path = require('path');
const E = require('./engine.js');

const fx = JSON.parse(fs.readFileSync(path.join(__dirname, 'scenarios.json'), 'utf8'));
const byId = {}, actions = {};
fx.cards.forEach((c) => { byId[c.id] = c; });
fx.actions.forEach((a) => { actions[a.id] = a; });
let steps = 0, bad = 0;
// JSON with sorted keys, so the comparison ignores key order.
const canon = (v) => JSON.stringify(v, (k, x) => (x && typeof x === 'object' && !Array.isArray(x)
  ? Object.fromEntries(Object.keys(x).sort().map((key) => [key, x[key]])) : x));

for (const sc of fx.scenarios) {
  const svc = new E.Service();
  const play = new E.Play();
  svc.allowSandbox = !!sc.allowSandbox;
  svc.resolver = sc.resolver || null;
  sc.steps.forEach((st, i) => {
    let t;
    if (st.db) { svc.dbDown = st.db === 'down'; return; }
    if ('playDown' in st) { play.down = st.playDown; return; }
    if (st.ack) { play.ackDown = st.ack === 'down'; return; }
    if (st.register) { const c = byId[st.register]; play.register(c, E.prepare(c)); return; }
    if (st.do) t = E.runAction(svc, actions[st.do]);
    else t = E.deliver(svc, byId[st.send], { wallClock: E.d(fx.wallClock), play });
    const got = E.outcome(t, E.snapshot(svc, fx.user, fx.checkProducts, fx.checkDates));
    const want = st.expect;
    const name = st.send || st.do;
    steps++;
    if (!want) { bad++; console.error(`${sc.name} #${i} ${name}: no expectation (run the Go test with -update-site)`); return; }
    if (canon(got) !== canon(want)) {
      bad++;
      console.error(`MISMATCH ${sc.name} #${i} ${name}\n  js:   ${canon(got)}\n  want: ${canon(want)}`);
    }
  });
}
console.log(`${steps} steps in ${fx.scenarios.length} scenarios, ${bad} mismatches`);
process.exit(bad ? 1 : 0);
