import { readFileSync } from 'node:fs';
import vm from 'node:vm';
import assert from 'node:assert/strict';
const source = readFileSync(new URL('../internal/api/static/app.js', import.meta.url), 'utf8');
const nodes = Object.fromEntries(['issueDialog', 'issueDialogTitle', 'issueDialogMeta', 'issueDialogContent', '.app'].map(id => [id, { open: false, showModal() { this.open = true; }, close() { this.open = false; }, set textContent(value) { this.innerHTML = value; } }]));
const pending = [];
const sandbox = vm.createContext({
 state: { issuePage: { issues: [{ id: 42, title: 'summary', kind: 'manual', state: 'open' }] } },
 document: { getElementById: id => nodes[id], querySelector: id => nodes[id] },
 api: url => new Promise((resolve, reject) => pending.push({ url, resolve, reject })),
 esc: value => String(value), renderMarkdown: value => `rendered:${value}`, fmtTime: String, repoName: String,
});
const start = source.includes('async function openIssue(') ? source.indexOf('async function openIssue(') : source.indexOf('function openIssue(');
vm.runInContext(source.slice(start, source.indexOf('/* ---------- status ---------- */')), sandbox);
const first = vm.runInContext('openIssue(42)', sandbox);
assert.equal(pending[0]?.url, '/api/issues/42');
assert.equal(nodes.issueDialog.open, true);
pending[0].resolve({ body: 'body from detail endpoint' });
await first;
assert.match(nodes.issueDialogContent.innerHTML, /rendered:body from detail endpoint/);
const second = vm.runInContext('openIssue(42)', sandbox);
vm.runInContext('closeIssue()', sandbox);
const before = nodes.issueDialogContent.innerHTML;
pending[1].resolve({ body: 'late body' });
await second;
assert.equal(nodes.issueDialogContent.innerHTML, before, 'closed dialog discards late detail');
const third = vm.runInContext('openIssue(42)', sandbox);
pending[2].reject(new Error('detail denied'));
await third;
assert.match(nodes.issueDialogContent.innerHTML, /detail denied/);
assert.doesNotMatch(nodes.issueDialogContent.innerHTML, /\(empty\)/);
