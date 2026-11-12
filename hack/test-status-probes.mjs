import { readFileSync } from 'node:fs';
import vm from 'node:vm';
import assert from 'node:assert/strict';

const source = readFileSync(new URL('../internal/api/static/app.js', import.meta.url), 'utf8');
const sandbox = vm.createContext({console, URLSearchParams, Map, Set, Date, location: {pathname: '/status'}, document: {body: {dataset: {version: 'test'}}, getElementById() { return null; }}, localStorage: {getItem() { return null; }}, setInterval() {}, clearInterval() {}, requestAnimationFrame() { return 1; }, cancelAnimationFrame() {}});
vm.runInContext(source.slice(0, source.indexOf('/* ---------- events ---------- */')), sandbox);
vm.runInContext(`
 let statusHTML = '';
 replaceApp = html => { statusHTML = html; };
 api = async path => path === '/api/status' ? {vcs: 'ready', upstream_info: [{name: 'forge', kind: 'ssh', base_url: 'ssh://git@forge.example/org', probe: 'ready', last_error: 'handshake <timeout>', checked_at: '2026-01-01T00:00:00Z'}]} : [];
`, sandbox);
await vm.runInContext('renderStatus(0)', sandbox);
const html = vm.runInContext('statusHTML', sandbox);
assert.ok(html.includes('handshake &lt;timeout&gt;'), 'last error remains visible while state stays ready');
assert.ok(!html.includes('handshake <timeout>'), 'probe error must be escaped');
assert.ok(html.includes('checked '), 'retain observation time in diagnostic detail');
assert.ok(html.includes('reachable'), 'transient error retains ready state');
console.log('Upstream status retains state, last error and check time');
