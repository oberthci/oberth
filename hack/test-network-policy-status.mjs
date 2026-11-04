import { readFileSync } from 'node:fs';
import vm from 'node:vm';
import assert from 'node:assert/strict';

const source = readFileSync(new URL('../internal/api/static/app.js', import.meta.url), 'utf8');
const sandbox = vm.createContext({console, URLSearchParams, Map, Set, Date, location: {pathname: '/status'}, document: {body: {dataset: {version: 'test'}}, getElementById() { return null; }}, localStorage: {getItem() { return null; }}, setInterval() {}, clearInterval() {}, requestAnimationFrame() { return 1; }, cancelAnimationFrame() {}});
vm.runInContext(source.slice(0, source.indexOf('/* ---------- events ---------- */')), sandbox);
vm.runInContext(`
 let statusHTML = '';
 let policyDetail = 'WARNING: pipeline NetworkPolicy is configured disabled; <configuration>';
 replaceApp = html => { statusHTML = html; };
 api = async path => path === '/api/status' ? {cluster: 'ready', cluster_detail: policyDetail} : [];
`, sandbox);
for (const detail of ['WARNING: pipeline NetworkPolicy is configured disabled; <configuration>', 'pipeline NetworkPolicy is configured enabled; CNI enforcement has not been measured', 'pipeline NetworkPolicy configuration is unavailable; CNI enforcement has not been measured']) {
  vm.runInContext(`policyDetail = ${JSON.stringify(detail)};`, sandbox);
  await vm.runInContext('renderStatus(0)', sandbox);
  const html = vm.runInContext('statusHTML', sandbox);
  assert.ok(html.includes(detail.replaceAll('<', '&lt;').replaceAll('>', '&gt;')), detail);
  assert.ok(!html.includes('<configuration>'), 'detail must be escaped');
  assert.equal((html.match(/class="svc /g) || []).length, 6, 'keep existing service cards');
}
console.log('NetworkPolicy diagnostic renders in the existing Kubernetes card');
