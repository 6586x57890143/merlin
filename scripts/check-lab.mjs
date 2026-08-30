// Drives the real lab.wasm headlessly.
//
// internal/lab's Go tests cover the decisions; this covers the part they
// cannot reach, which is the compiled binary and the JSON marshalling in
// cmd/lab. That is a small surface, but it is the only code between a working
// Go function and a page whose buttons do nothing.
//
//   bash scripts/build-lab.sh && node scripts/check-lab.mjs web/lab
//
// Deliberately not wired into CI: it would add Node to a Go pipeline for
// forty lines of glue. CI builds cmd/lab for js/wasm instead, which catches
// the drift that actually happens (a signature moving under the binding).
import fs from 'node:fs';
import { EventEmitter } from 'node:events';
import path from 'node:path';
import { createRequire } from 'node:module';

const require = createRequire(import.meta.url);
// Resolved absolutely: require() treats a bare relative path as a package
// name, so `node scripts/check-lab.mjs web/lab` would look for a module.
const dir = path.resolve(process.argv[2] ?? 'web/lab');

// The page gets Event/dispatchEvent from the DOM; Node needs them shimmed.
const bus = new EventEmitter();
globalThis.Event = globalThis.Event || class { constructor(type) { this.type = type; } };
globalThis.dispatchEvent = (e) => { bus.emit(e.type); return true; };
globalThis.addEventListener = (t, fn) => bus.on(t, fn);

require(`${dir}/wasm_exec.js`);

const ready = new Promise((resolve, reject) => {
  globalThis.addEventListener('merlin-lab-ready', () => resolve());
  globalThis.addEventListener('merlin-lab-failed', () => reject(new Error(globalThis.merlinLabError)));
  setTimeout(() => reject(new Error('timed out waiting for the lab to signal ready')), 30000);
});

const go = new Go();
const bytes = fs.readFileSync(`${dir}/lab.wasm`);
const result = await WebAssembly.instantiate(bytes, go.importObject);
go.run(result.instance);
await ready;

const call = (fn, payload) => JSON.parse(globalThis.merlinLab[fn](JSON.stringify(payload ?? {})));

let failures = 0;
const check = (name, ok, detail) => {
  console.log(`${ok ? 'ok  ' : 'FAIL'}  ${name}${ok ? '' : '  <- ' + detail}`);
  if (!ok) failures++;
};

// --- rotation ---
const rot = call('rotation', {
  interval: '90m', leadMinutes: 10, disclosure: 'full', retentionHours: 72, fires: 3,
});
check('rotation returns no error', !rot.error, rot.error);
check('rotation lists 3 fire instants', rot.fires?.length === 3, JSON.stringify(rot.fires));
check('rotation renders a heads-up', !!rot.headsUp, '(empty)');
check('rotation renders an intro notice', !!rot.intro, '(empty)');
console.log('      cadence :', rot.cadence);
console.log('      fires   :', (rot.fires || []).join('  '));
console.log('      headsUp :', rot.headsUp);
console.log('      intro   :', rot.intro);

const bad = call('rotation', { interval: '5m', disclosure: 'full' });
check('an interval below the floor is refused', !!bad.error, 'accepted');
console.log('      refusal :', bad.error);

const generic = call('rotation', {
  interval: '24h', leadMinutes: 10, disclosure: 'generic', retentionHours: 72,
});
const leaked = ['1 day', '24 hours', '3 days', '72 hours'].filter((s) => (generic.intro || '').includes(s));
check('generic disclosure leaks neither figure', leaked.length === 0, leaked.join(','));
console.log('      generic :', generic.intro);

// --- voice ---
const keys = call('keys');
check('keys returns the catalog contract', Array.isArray(keys) && keys.length > 0, JSON.stringify(keys).slice(0, 120));
const introKey = (keys || []).find((k) => k.key === 'rotation.intro.full');
check('rotation.intro.full requires two placeholders', introKey?.required?.length === 2, JSON.stringify(introKey));

const rolled = call('roll', {
  key: 'rotation.intro.full', vars: { cadence: '24 hours', retention: '3 days' }, n: 6,
});
check('roll returns 6 lines', rolled.length === 6, JSON.stringify(rolled).slice(0, 120));
check('rolled lines vary', new Set(rolled).size > 1, 'all identical');
rolled.slice(0, 3).forEach((l) => console.log('      line    :', l));

const clean = call('lint', { key: 'rotation.intro.full', line: 'resets every {cadence}, kept {retention}.' });
check('a conforming line lints clean', clean.length === 0, JSON.stringify(clean));
const missing = call('lint', { key: 'rotation.intro.full', line: 'resets every {cadence}.' });
check('a line missing {retention} is refused', missing.length > 0, 'accepted');
console.log('      lint    :', missing[0]);

// --- error shape ---
const broken = JSON.parse(globalThis.merlinLab.rotation('not json'));
check('malformed input comes back as {error}', !!broken.error, JSON.stringify(broken));

console.log(failures === 0 ? '\nALL LAB CHECKS PASSED' : `\n${failures} CHECK(S) FAILED`);
process.exit(failures === 0 ? 0 : 1);
