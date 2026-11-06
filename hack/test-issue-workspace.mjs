import { readFileSync } from 'node:fs';
import vm from 'node:vm';
import assert from 'node:assert/strict';
const source = readFileSync(new URL('../internal/api/static/app.js', import.meta.url), 'utf8');
const sandbox = vm.createContext({
 state: { issueWorkStatus: 'active', issueState: 'open', repos: [] }, URLSearchParams,
 esc: value => String(value).replaceAll('<', '&lt;'), pill: value => String(value),
 repoName: String, fmtTime: String, ago: String, shortStamp: String, stampTitle: String,
});
vm.runInContext(source.slice(source.indexOf('function issueListURL()'), source.indexOf('async function renderIssues(')), sandbox);
assert.match(vm.runInContext('issueListURL()', sandbox), /work_status=active/);
const html = vm.runInContext('issueRow({ID:1, Kind:"manual", State:"open", work_status:"active", stale:true, Title:"<script>"})', sandbox);
assert.doesNotMatch(html, /active/);
assert.match(html, /stale/);
assert.doesNotMatch(html, /<script>/);
const fresh = vm.runInContext('issueRow({ID:2, State:"closed", work_status:"review", stale:false})', sandbox);
assert.doesNotMatch(fresh, /review/);
assert.doesNotMatch(fresh, />stale</);
