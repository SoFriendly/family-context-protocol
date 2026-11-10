import assert from 'node:assert/strict';
import { existsSync, readFileSync } from 'node:fs';
import { runInNewContext } from 'node:vm';

const root = new URL('../site/public/', import.meta.url);
const html = readFileSync(new URL('index.html', root), 'utf8');
const ids = new Set([...html.matchAll(/id="([^"]+)"/g)].map(([, id]) => id));
for (const [, target] of html.matchAll(/href="#([^"]+)"/g)) assert.ok(ids.has(target), `Missing anchor: ${target}`);
for (const [, src] of html.matchAll(/(?:src|href)="(\/(?:assistants\/[^" ]+|site\.js|logo\.svg))"/g)) {
  assert.ok(existsSync(new URL(src.slice(1), root)), `Missing local asset: ${src}`);
}
assert.doesNotMatch(html, /id="(?:advance|demo-status|packet)"|\/demo\.js/, 'Retired demo controls must not remain');
assert.match(html, /Without FCP/);
assert.match(html, /With FCP/);
for (const name of ['ChatGPT Dot', 'Claude Duet', 'Grok Bot', 'Muse']) assert.ok(html.includes(name));
for (const f of ['darwin-arm64', 'darwin-amd64', 'linux-arm64', 'linux-arm', 'linux-amd64']) assert.ok(html.includes(`/dl/latest/fcp-${f}"`), `Missing download button: ${f}`);

const script = readFileSync(new URL('site.js', root), 'utf8');
// Reduced motion must never register a reveal that could hide content.
runInNewContext(script, { window: { matchMedia: () => ({ matches: true }), IntersectionObserver: true } });
let callback;
let observed = 0;
let revealed = 0;
let released = 0;
const element = { classList: { add: () => revealed++ } };
runInNewContext(script, {
  window: { matchMedia: () => ({ matches: false }), IntersectionObserver: true },
  document: { querySelectorAll: () => [element] },
  IntersectionObserver: class {
    constructor(fn) { callback = fn; }
    observe() { observed++; }
    unobserve() { released++; }
  },
});
assert.equal(observed, 1);
callback([{ isIntersecting: false, target: element }]);
assert.equal(revealed, 0);
callback([{ isIntersecting: true, target: element }]);
assert.equal(revealed, 1);
assert.equal(released, 1);
console.log('PASS: anchors, local assets, before/after content, retired demo cleanup, reduced motion and reveal lifecycle');
