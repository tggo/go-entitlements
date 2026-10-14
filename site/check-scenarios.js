#!/usr/bin/env node
/* Runs engine.js (the simulator's JS port) over site/scenarios.json and checks
 * every step against the "expect" blocks that the Go test wrote from the real
 * library. A mismatch means the port drifted from the Go code. */
'use strict';
const fs = require('fs');
const path = require('path');
const E = require('./engine.js');

const fx = JSON.parse(fs.readFileSync(path.join(__dirname, 'scenarios.json'), 'utf8'));
const byId = {};
fx.cards.forEach((c) => { byId[c.id] = c; });
let steps = 0, bad = 0;
// JSON with sorted keys, so the comparison ignores key order.
const canon = (v) => JSON.stringify(v, (k, x) => (x && typeof x === 'object' && !Array.isArray(x)
  ? Object.fromEntries(Object.keys(x).sort().map((key) => [key, x[key]])) : x));

for (const sc of fx.scenarios) {
  const svc = new E.Service();
  svc.allowSandbox = !!sc.allowSandbox;
  let playDown = false;
  sc.steps.forEach((st, i) => {
    if (st.db) { svc.dbDown = st.db === 'down'; return; }
    if ('playDown' in st) { playDown = st.playDown; return; }
    const t = E.deliver(svc, byId[st.send], { wallClock: E.d(fx.wallClock), playDown });
    const got = { code: t.code, stamp: t.stamp, ...E.snapshot(svc, fx.user, fx.product, fx.checkDates) };
    const want = st.expect;
    steps++;
    if (!want) { bad++; console.error(`${sc.name} #${i} ${st.send}: no expectation (run the Go test with -update-site)`); return; }
    if (canon(got) !== canon(want)) {
      bad++;
      console.error(`MISMATCH ${sc.name} #${i} ${st.send}\n  js:   ${canon(got)}\n  want: ${canon(want)}`);
    }
  });
}
console.log(`${steps} steps in ${fx.scenarios.length} scenarios, ${bad} mismatches`);
process.exit(bad ? 1 : 0);
