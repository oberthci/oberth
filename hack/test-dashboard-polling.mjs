// Polling, focus and accessibility regressions against the shipped script.
import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import vm from 'node:vm';
const source = readFileSync(process.env.DASHBOARD_SCRIPT || new URL('../internal/api/static/app.js', import.meta.url), 'utf8');
const intervals = [];
const fakeApp = {innerHTML: '', focusTargets: [], contains(node) { return node !== null && node !== undefined && node !== 'outside'; }, querySelectorAll(selector) { return selector === '[data-run-id]' ? this.focusTargets : []; }};
const doc = {hidden: false, activeElement: null, body: {dataset: {version: 'test'}}, getElementById(id) { return id === 'app' ? fakeApp : null; }, querySelector() { return null; }};
const sandbox = vm.createContext({URLSearchParams, URL, console, Map, Set, Date, structuredClone, AbortController, location: {pathname: '/runs'}, document: doc,
  window: {getSelection() { return sandbox.selection; }}, localStorage: {getItem(name) { return name === 'oberth-token' ? 'token' : null; }, setItem() {}, removeItem() {}},
  setInterval(fn, ms) { intervals.push({fn, ms}); return intervals.length; }, clearInterval() {}});
vm.runInContext(source.slice(0, source.indexOf('/* ---------- events ---------- */')), sandbox);
const run = code => vm.runInContext(code, sandbox);

// Conditional polls: the second request names the ETag and a 304 reuses the body and cursor.
let sent = [];
sandbox.fetch = async (url, init) => {
  sent.push({url, match: init.headers['If-None-Match'] || ''});
  const headers = new Headers({'ETag': 'W/"v1"', 'X-Oberth-Next-Before': '41'});
  if (init.headers['If-None-Match'] === 'W/"v1"') return {status: 304, ok: false, headers, json: async () => { throw new Error('a 304 has no body'); }};
  return {status: 200, ok: true, headers, json: async () => [{id: 'run-1', status: 'passed', display_status: 'passed'}]};
};
const first = await run(`api('/api/runs?limit=100', {withNext: true})`);
const second = await run(`api('/api/runs?limit=100', {withNext: true})`);
assert.equal(sent[0].match, '', 'the first poll has no validator');
assert.equal(sent[1].match, 'W/"v1"', 'the next poll sends If-None-Match');
assert.deepEqual(JSON.parse(JSON.stringify(second)), JSON.parse(JSON.stringify(first)), 'a 304 reuses the cached body and cursor');
second.body[0].status = 'edited';
assert.equal((await run(`api('/api/runs?limit=100', {withNext: true})`)).body[0].status, 'passed', 'callers get a copy, never the cache itself');
run(`setToken('other')`);
sent = [];
await run(`api('/api/runs?limit=100', {withNext: true})`);
assert.equal(sent[0].match, '', 'a token change drops the cached views');
console.log('Conditional polling passed: If-None-Match, 304 reuse, copies and cache reset');

// Hidden tabs do not poll.
run(`route = async () => { globalThis.routedCount = (globalThis.routedCount || 0) + 1; }; setAuto(4000);`);
doc.hidden = true; intervals.at(-1).fn();
assert.equal(run('globalThis.routedCount || 0'), 0, 'a hidden tab must not poll');
doc.hidden = false; intervals.at(-1).fn();
assert.equal(run('globalThis.routedCount'), 1, 'a visible tab polls');
console.log('Hidden-tab backoff passed');

// Background renders keep typing, selections and focus.
const element = (attrs, tagName = 'BUTTON') => ({tagName, attrs, focused: false, hasAttribute(name) { return name in this.attrs; }, getAttribute(name) { return this.attrs[name]; }, focus() { this.focused = true; }});
const fresh = element({'data-run-id': 'run-1'});
fakeApp.innerHTML = 'old'; fakeApp.focusTargets = [fresh];
run(`state.routeSeq = 7; state.backgroundSeq = 7;`);
doc.activeElement = element({'data-action': 'issue-query'}, 'INPUT');
run(`replaceApp('new')`);
assert.equal(fakeApp.innerHTML, 'old', 'a poll must not rebuild the page under a focused input');
doc.activeElement = null; sandbox.selection = {isCollapsed: false, anchorNode: 'inside'};
run(`replaceApp('new')`);
assert.equal(fakeApp.innerHTML, 'old', 'a poll must not wipe a text selection');
sandbox.selection = null; doc.activeElement = element({'data-run-id': 'run-1'});
run(`replaceApp('new')`);
assert.equal(fakeApp.innerHTML, 'new', 'a poll renders when the reader is not busy');
assert.ok(fresh.focused, 'the focused row is focused again after the poll');
run(`state.backgroundSeq = 0;`);
doc.activeElement = element({'data-action': 'issue-query'}, 'INPUT');
run(`replaceApp('navigated')`);
assert.equal(fakeApp.innerHTML, 'navigated', 'an explicit navigation always renders');
console.log('Focus and selection survive polling');

// Rows are keyboard links and read only snake_case fields.
run(`state.repos = [{id: 1, name: 'app', upstream_id: 1}]; state.status = {upstream_info: [{id: 1, name: 'github', base_url: 'https://example.test/org'}]};`);
const row = run(`runRow({id: 'abc12345', repo_id: 1, ref: 'feature/x', ref_kind: 'branch', status: 'passed', display_status: 'passed', started_at: '2026-11-01T10:00:00Z', finished_at: '2026-11-01T10:01:00Z'})`);
assert.match(row, /class="trow" data-run-id="abc12345" tabindex="0" role="link" aria-label="run app feature\/x passed"/);
assert.match(row, /feature\/x/);
assert.match(source, /event\.target\.matches\?\.\('\[role="link"\]\[tabindex\], \[role="button"\]\[tabindex\]'\)/, 'Enter opens row links');
console.log('Keyboard rows passed');

// The runs page reads the repository summary, once per slow poll.
const requested = [];
run(`api = async path => { globalThis.requestedPaths.push(path); if (path.startsWith('/api/repos')) return [{id: 1, name: 'app', upstream_id: 1}]; if (path === '/api/runs/counts') return {total: 0, by_repo: {}}; if (path === '/api/status') return state.status; if (/[?&]limit=1(&|$)/.test(path)) return []; return {body: [], next: ''}; };`);
sandbox.requestedPaths = requested;
run(`state.repos = []; replaceApp = () => {};`);
await run('renderRuns(state.routeSeq)');
await run('renderRuns(state.routeSeq)');
const repoReads = requested.filter(path => path.startsWith('/api/repos'));
assert.deepEqual(repoReads, ['/api/repos?view=summary'], 'the runs page reads the summary view at most once per slow poll');
console.log('Runs page repository polling passed');
