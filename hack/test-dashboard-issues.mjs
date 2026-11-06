// Issue-list regressions against the shipped script; no server or browser required.
import assert from 'node:assert/strict';
import {readFileSync, writeFileSync} from 'node:fs';
import vm from 'node:vm';
process.env.TZ = 'UTC';
const now = Date.UTC(2026, 10, 6, 15, 30);
class FixedDate extends Date {
  constructor(...args) { super(...(args.length ? args : [now])); }
  static now() { return now; }
}
const source = readFileSync(process.env.DASHBOARD_SCRIPT || new URL('../internal/api/static/app.js', import.meta.url), 'utf8');
const sandbox = vm.createContext({URLSearchParams, console, Map, Set, Date:FixedDate, location:{pathname:'/issues'}, document:{body:{dataset:{version:'test'}},getElementById(){return null}}, localStorage:{getItem(){return null},setItem(){}}, setInterval(){return 1},clearInterval(){}});
vm.runInContext(source.slice(0, source.indexOf('/* ---------- events ---------- */')), sandbox);
const run = code => vm.runInContext(code, sandbox);
run(`
 let html='', requests=[];
 replaceApp=value=>{html=value};setChrome=()=>{};setAuto=()=>{};rerenderWhenStatus=()=>{};
 state.repos=[{ID:1,Name:'ours',UpstreamID:1}];
 state.status={upstream_info:[{id:1,name:'codeberg',base_url:'https://example.test/acme'}]};
 ensureStatus=()=>Promise.resolve(state.status);
 const baseIssue={ID:42,RepoID:1,upstream:'codeberg',Kind:'ci',State:'open',Title:'Build needs attention',Branch:'feature/fix',CreatedAt:'2026-11-06T09:07:00Z',UpdatedAt:'2026-11-05T17:08:00Z',Occurrences:12,work_status:'active'};
 const rows=[baseIssue,{...baseIssue,ID:43,RepoID:0,Kind:'manual',Title:'Upstream setting'}, {...baseIssue,ID:44,RepoID:0,upstream:'',Kind:'manual',Title:'Installation setting'}, {...baseIssue,ID:45,State:'closed',Title:'Resolved issue'}, {...baseIssue,ID:46,stale:true,Title:'Needs follow-up'}];
 api=async path=>{requests.push(path);return {Issues:rows,counts:{open:4,closed:1,manual:2,ci:3},total:5,NextBefore:42}};
`);
assert.equal(run('state.issueState'),'open');
for (const [stamp, expected] of [
 ['2026-11-06T09:07:00Z','09:07'],
 ['2026-11-05T17:08:00Z','Nov 5 17:08'],
 ['2025-11-05T17:08:00Z','Nov 5 2025 17:08'],
 ['2027-01-02T00:01:00Z','Jan 2 2027 00:01'],
 ['', '--'],['invalid','--'],[null,'--'],
]) assert.equal(run(`shortStamp(${JSON.stringify(stamp)})`),expected);
const repoRow=run('issueRow(baseIssue)');
assert.match(repoRow,/codeberg \/ ours/);
assert.match(repoRow,/title="codeberg\/acme\/ours"/);
assert.match(repoRow,/class="issue-branch"[^>]*>feature\/fix/);
assert.match(repoRow,/class="issue-filed"[^>]*>09:07<\/span>/);
assert.match(repoRow,/class="issue-worked"[^>]*>Nov 5 17:08<\/span>/);
assert.match(repoRow,/title="[^"<>]+ · \d+[smhd] ago"/);
assert.doesNotMatch(repoRow,/pill|issue-state|occurrences|×|lock|class="sha"/);
const upstream=run('issueRow(rows[1])'), installation=run('issueRow(rows[2])');
assert.match(upstream,/class="clip">codeberg<\/span>/);
assert.doesNotMatch(upstream,/codeberg \/|installation|issue-branch/);
assert.match(installation,/class="clip">installation<\/span>/);
const escaped=run(`issueRow({...baseIssue,Title:'<script>"bad"',Branch:'<branch>'})`);
assert.doesNotMatch(escaped,/<script>|<branch>/);
assert.match(escaped,/title="&lt;script&gt;&quot;bad&quot;"/);
for(const state of ['open','closed','']) {
 run(`state.issueState=${JSON.stringify(state)}`);
 const row=run('issueRow(rows[3])');
 assert.equal(row.includes('is-closed'),state==='');
 assert.equal(/>closed Nov 5 17:08</.test(row),state==='');
 const stale=run('issueRow(rows[4])');
 assert.equal((stale.match(/ stale</g)||[]).length,1);
 assert.doesNotMatch(stale,/pill|is-closed/);
}
await run('renderIssues(0)');
const page=run('html');
assert.match(page,/<span>#<\/span><span>where<\/span><span>title<\/span><span>filed<\/span><span>last worked<\/span>/);
assert.doesNotMatch(page,/data-issue-kind|state \/ work|<span>seen<\/span>| manual ·| CI ·/);
assert.equal((page.match(/data-issue-state=/g)||[]).length,3);
for(const control of ['issue-repo','issue-work-status','issue-query','issue-newer','issue-older'])assert.ok(page.includes(control));
assert.match(page,/value="codeberg\/acme\/ours"/);
run(`state.issueRepo='codeberg/acme/ours';state.issueState='open';state.issueWorkStatus='active';state.issueQuery='needle';state.issueCursor=42`);
const query=new URL('https://example.test'+run('issueListURL()')).searchParams;
assert.equal(query.get('repo'),'codeberg/acme/ours');assert.equal(query.get('state'),'open');assert.equal(query.get('work_status'),'active');assert.equal(query.get('query'),'needle');assert.equal(query.get('before'),'42');assert.equal(query.has('kind'),false);
const when=run(`whenCell({display_status:'passed',StartedAt:baseIssue.UpdatedAt,FinishedAt:baseIssue.UpdatedAt})`);
assert.ok(when.includes(run('shortStamp(baseIssue.UpdatedAt)')));
assert.match(when,/title="[^"<>]+ · \d+[smhd] ago"/);
if(process.env.DASHBOARD_ISSUES_PREVIEW) {
 const css=readFileSync(new URL('../internal/api/static/app.css',import.meta.url),'utf8');
 writeFileSync(process.env.DASHBOARD_ISSUES_PREVIEW,`<!doctype html><meta name="viewport" content="width=device-width,initial-scale=1"><title>Issues fixture</title><style>${css}</style>${page}`);
}
console.log('Issues list passed: scope, five columns, neutral rows, conditional closed markers, shared dates, escaping and filters');
