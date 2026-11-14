// Execute the shipped dashboard functions with deterministic browser/API mocks.
import { readFileSync } from 'node:fs';
import vm from 'node:vm';
import assert from 'node:assert/strict';
const source = readFileSync(new URL('../internal/api/static/app.js', import.meta.url), 'utf8');
const sandbox = vm.createContext({ console, URLSearchParams, Map, Set, Date, location:{pathname:'/runs/live-run'}, document: {getElementById(){return null;}}, localStorage:{getItem(){return null;}}, AbortController, setTimeout() {}, clearTimeout() {}, setInterval() {}, clearInterval() {}, requestAnimationFrame() {return 1;}, cancelAnimationFrame() {} });
vm.runInContext(source.slice(0, source.indexOf('/* ---------- events ---------- */')), sandbox);
const run = code => vm.runInContext(code, sandbox);
run(`repoName = () => 'oberth';`);
let html;
assert.ok(run(`renderLogLines(['@@oberth:step plain', '::oberth-log:: plain', '[build/one] command'])`).includes('::oberth-log:: plain'));
// Equal display names from fan-out and retries retain separate selection keys.
run(`const nativeRows = [
 {node_id:'attempt-1',display_name:'check',template:'shared',phase:'Succeeded',status:'passed',ordinal:1},
 {node_id:'branch-0',display_name:'check',template:'shared',phase:'Failed',message:'native detail',status:'failed',ordinal:0},
 {node_id:'omitted',display_name:'check',template:'shared',phase:'Omitted',status:'skipped',ordinal:2}
];`);
assert.equal(run(`orderSteps(nativeRows).map(row => row.key).join(',')`),'branch-0,attempt-1,omitted');
html = run(`orderSteps(nativeRows).map(row => stepRow(row,'attempt-1')).join('')`);
assert.equal((html.match(/data-select-step=/g)||[]).length,3);
assert.ok(html.includes('data-select-step="attempt-1"') && html.includes('native detail') && html.includes('Omitted'));
assert.equal(run(`orderSteps([]).length`),0);
assert.equal(run(`orderSteps(null).length`),0);

// Failure selection follows the recorded native node, not the earliest retry.
assert.equal(run(`defaultStep({run:{status:'failed',failed_step:'attempt-1'},steps:nativeRows})`),'attempt-1');
assert.equal(run(`defaultStep({run:{status:'passed'},steps:nativeRows})`),'');
// Selecting a node with no copy must not silently show a sibling's bytes.
run(`api=async path=>path.endsWith('/containers')?{containers:[{node_id:'other',container:'main',stored:true}]}:{output:'selected node reason'}; awaitResult=containerLogContent({id:'missing',status:'failed'},'target');`);
html=await run('awaitResult');
assert.ok(html.html.includes('selected node reason'));

// Exercise the actual detail renderer's scroll restoration and combined-view
// selection, including a terminal poll that must continue draining chunks.
run(`
  state.repos = [{id: 1, name:'oberth'}];
  state.stepRunID = 'scroll-run'; state.step = '__summary__';
  currentRoute = () => true; setChrome = () => {}; replaceApp = () => {};
  containerLogContent = async () => ({html:'fixture log',options:'',live:true}); state.live = {key:'fixture',controller:new AbortController(),done:false}; paintContainerLog = () => {};
  const oldTerm = {scrollHeight:1000, scrollTop:123, clientHeight:100};
  const oldSteps = {scrollTop:45};
  const newTerm = {scrollHeight:1200, scrollTop:0, clientHeight:100,dataset:{}};
  const newSteps = {scrollTop:0};
  let termReads = 0, stepReads = 0;
  document = {getElementById(id) {
    if(id==='termBody') return termReads++ === 0 ? oldTerm : newTerm;
    if(id==='stepList') return stepReads++ === 0 ? oldSteps : newSteps;
    return null;
  }};
  setAuto = () => {};
  awaitResult = renderRunDetailView({run:{id:'scroll-run', repo_id:1, status:'running', display_status:'running', ref:'fix/live'}, steps:[]}, 0);
`);
await run('awaitResult');
assert.equal(run('newTerm.scrollTop'), 123);
assert.equal(run('newSteps.scrollTop'), 45);
run(`termReads=0; stepReads=0; oldTerm.scrollTop=900; awaitResult=renderRunDetailView({run:{id:'scroll-run',repo_id:1,status:'running',display_status:'running',started_at:new Date().toISOString(),ref:'fix/live'},steps:[]},0);`);
await run('awaitResult');
assert.equal(run('newTerm.scrollTop'), 1200);

run(`
  let renderedHTML = '';
  replaceApp = html => { renderedHTML = html; };
  state.step = 'build / compile';
  state.stepRunID = 'scroll-run';
  termReads=0; stepReads=0;
  awaitResult=renderRunDetailView({run:{id:'scroll-run',repo_id:1,status:'running',display_status:'running',started_at:new Date().toISOString(),ref:'fix/live'},steps:[{burn:'build',step:'compile',status:'queued',ordinal:0}]},0);
`);
await run('awaitResult');
assert.ok(run('renderedHTML').includes('fixture log'), 'a queued pod can emit before Argo publishes Running');

// Empty row messages identify schedules, escaped; detail keeps its schedule badge.
run(`const scheduleRun = {id:'scroll-run', repo_id:1, status:'passed', display_status:'passed', ref:'main', trigger:'schedule', schedule_entry:'nightly<unsafe>', display_reason:'schedule nightly<unsafe>'};`);
html = run(`runsTable([scheduleRun, {...scheduleRun, id:'weekly-run', schedule_entry:'weekly-sweep', display_reason:'schedule weekly-sweep'}])`);
assert.ok(html.includes('schedule nightly&lt;unsafe&gt;') && html.includes('schedule weekly-sweep'));
assert.ok(!html.includes('nightly<unsafe>'));
assert.equal(run(`scheduleBadge({...scheduleRun, trigger:'branch'})`), '');
assert.equal(run(`scheduleBadge({...scheduleRun, schedule_entry:''})`), '');
run(`state.step='__summary__'; termReads=0; stepReads=0; awaitResult=renderRunDetailView({run:scheduleRun,steps:[]},0);`);
await run('awaitResult');
assert.ok(run('renderedHTML').includes('title="schedule entry">nightly&lt;unsafe&gt;'));
assert.ok(run(`summaryLines({run:scheduleRun,steps:[]}).join(' ')`).includes('schedule=nightly<unsafe>'));

// The deadline is a running run's reason, never a status of its own.
assert.equal(run(`statusKind('deadline-exceeded')`), 'unknown');
assert.equal(run(`displayRunStatus({status:'running',started_at:'2026-10-30T09:00:00Z',display_status:'running',display_reason:'Execution deadline exceeded.'})`), 'running');
const deadlineSummary = run(`summaryLines({run:{id:'deadline',status:'running',display_status:'running',display_reason:'Execution deadline exceeded.'},steps:[],scheduling:{message:'Execution deadline exceeded.',execution:{state:'deadline_exceeded',phase:'Failed',reconciliation_lag_seconds:3080,deadline:{budget_seconds:5400,expires_at:'2026-10-30T10:33:37Z',remaining_seconds:0}}}}).join(' ')`);
for (const text of ['[running]', 'Execution deadline exceeded', '3080s', '0s remaining', 'clock: Workflow start']) assert.ok(deadlineSummary.includes(text), text);
assert.ok(!deadlineSummary.includes('[queued]'));
run(`state.step='build / compile'; state.stepRunID='scroll-run'; termReads=0; stepReads=0; awaitResult=renderRunDetailView({run:{id:'scroll-run',repo_id:1,status:'running',display_status:'running',display_reason:'Execution deadline exceeded.',started_at:new Date().toISOString(),ref:'fix/live'},steps:[{burn:'build',step:'compile',status:'running',ordinal:0}]},0);`);
await run('awaitResult');
assert.ok(run('renderedHTML').includes('fixture log') && run('renderedHTML').includes('live output'), 'an execution display label must not stop durable-running log refresh');
console.log('live log dashboard checks passed');

// Run-list status comes from the canonical server display field.
assert.equal(run(`displayRunStatus({status:'failed',display_status:'failed'})`), 'failed');
assert.equal(run(`displayRunStatus({status:'interrupted',display_status:'cancelled'})`), 'cancelled');
assert.equal(run(`displayRunStatus({status:'interrupted'})`), undefined, 'raw status cannot replace a missing canonical display status');
// The list always shows branch/tag heads, with history only on run detail.
assert.equal(run(`state.filter='all';runMatches({status:'interrupted',display_status:'cancelled'})`), true);
run(`state.attempts.clear();state.attempts.set('head',{rows:[{id:'old',repo_id:1,status:'interrupted',display_status:'cancelled'}],next:''});`);
const headTable = run(`runsTable([{id:'head',repo_id:1,status:'passed',display_status:'passed',ref:'feature',earlier_attempts:1}])`);
assert.ok(!headTable.includes('earlier attempts') && !headTable.includes('data-run-id="old"'));
assert.ok(run(`runHistory({id:'head',ref_kind:'branch'})`).includes('data-run-id="old"'), 'cancelled runs remain available in detail history');
assert.ok(run(`runHistory({id:'unloaded',ref_kind:'tag'})`).includes('Run history for this tag'));

run(`let rcaRequests=[]; api=async path=>{rcaRequests.push(path);return {body:[],next:''};};awaitResult=loadRuns();`);
await run('awaitResult');
assert.ok(run(`rcaRequests[0]`).includes('latest_per_ref=1'));
run(`state.repo='ours';state.runCursor='opaque-cursor';awaitResult=loadRepoRuns();`);
await run('awaitResult');
assert.ok(run(`rcaRequests[1]`).includes('latest_per_ref=1') && run(`rcaRequests[1]`).includes('before=opaque-cursor'));
run(`
 state.repos=[{id:1,name:'ours'},{id:2,name:'theirs'}];state.status={upstream_info:[]};
 // A directly loaded detail has no list-page head in memory.
 state.runs=[];state.repoRuns=[];state.attempts.clear();
 state.detailRun={id:'rca-head',repo_id:1,ref:'feature',ref_kind:'branch',status:'passed',display_status:'passed'};
 api=async path=>{rcaRequests.push(path);return {body:[state.detailRun,{id:'our-old',repo_id:1,ref:'feature',ref_kind:'branch',status:'interrupted',display_status:'cancelled'},{id:'tag-same-name',repo_id:1,ref:'feature',ref_kind:'tag'},{id:'other-repo',repo_id:2,ref:'feature',ref_kind:'branch'},{id:'other-ref',repo_id:1,ref:'elsewhere',ref_kind:'branch'}],next:'opaque-history-cursor'};};
 awaitResult=loadRunAttempts('rca-head',false);
`);
await run('awaitResult');
assert.equal(run(`JSON.stringify(state.attempts.get('rca-head').rows.map(row=>row.id))`), JSON.stringify(['our-old']));
assert.ok(!run(`rcaRequests.at(-1)`).includes('latest_per_ref'));
run(`api=async path=>{rcaRequests.push(path);return {body:[{id:'our-old',repo_id:1,ref:'feature',ref_kind:'branch'},{id:'older',repo_id:1,ref:'feature',ref_kind:'branch'}],next:''};};awaitResult=loadRunAttempts('rca-head',true);`);
await run('awaitResult');
assert.ok(run(`rcaRequests.at(-1)`).includes('before=opaque-history-cursor'));
assert.equal(run(`JSON.stringify(state.attempts.get('rca-head').rows.map(row=>row.id))`), JSON.stringify(['our-old','older']));
// A failed history page keeps loaded rows and offers a local retry.
run(`api=async()=>{throw new Error('<offline>')};awaitResult=loadRunAttempts('rca-head',true);`);
await run('awaitResult');
assert.ok(run(`runHistory(state.detailRun)`).includes('&lt;offline&gt;'));
assert.ok(run(`runHistory(state.detailRun)`).includes('Retry'));
assert.equal(run(`state.attempts.get('rca-head').rows.length`), 2);
await run(`loadRunAttempts('rca-head',false)`);
assert.equal(run(`state.attempts.has('rca-head')`), false);
// A collapsed or signed-out view must not be repopulated by a late response.
run(`let finishHistory;api=()=>new Promise(resolve=>{finishHistory=resolve});awaitResult=loadRunAttempts('rca-head',false);`);
await run(`loadRunAttempts('rca-head',false)`);
run(`finishHistory({body:[],next:''})`);
await run('awaitResult');
assert.equal(run(`state.attempts.has('rca-head')`), false);
run(`awaitResult=loadRunAttempts('rca-head',false);state.authGen++;state.attempts.clear();finishHistory({body:[],next:''});`);
await run('awaitResult');
assert.equal(run(`state.attempts.size`), 0);
console.log('run-list grouping and detail-history checks passed');

assert.equal(run(`summaryLines({run:{id:'original',status:'failed',display_status:'failed',display_reason:'infrastructure error'},steps:[]}).some(line => line.startsWith('[queued]'))`), false);

run(`
 const capacityStatusSavedAPI = api;
 let profileStatusCalls = 0;
 state.status = {version:'test'};
 state.statusLoadedAt = Date.now(); state.statusFailedAt = 0;
 api = async () => { profileStatusCalls++; return {version:'test'}; };
 statusRefreshPromise = ensureStatus();
`);
await run('statusRefreshPromise');
assert.equal(run('profileStatusCalls'), 0, 'fresh status must not be polled per log/run refresh');
run(`state.statusLoadedAt = Date.now()-31000; statusRefreshPromise = ensureStatus();`);
await run('statusRefreshPromise');
assert.equal(run('profileStatusCalls'), 0, '30s run refresh must not re-probe upstreams');
run(`state.statusLoadedAt = Date.now()-300001; statusRefreshPromise = ensureStatus(); secondRefreshPromise = ensureStatus();`);
assert.equal(run('statusRefreshPromise === secondRefreshPromise'), true, 'concurrent background refreshes coalesce');
await run('statusRefreshPromise');
assert.equal(run('profileStatusCalls'), 1);
run(`statusRefreshPromise=loadStatus();`);
await run('statusRefreshPromise');
assert.equal(run('profileStatusCalls'), 2, 'visiting Status must fetch fresh data during the background cooldown');
run(`state.statusLoadedAt=Date.now()-300001; api=async()=>{profileStatusCalls++;throw new Error('status unavailable')}; statusRefreshPromise=ensureStatus();`);
await run('statusRefreshPromise');
run('statusRefreshPromise=ensureStatus();');
await run('statusRefreshPromise');
assert.equal(run('profileStatusCalls'), 3, 'status failures retain the 30s retry backoff');
run('api=capacityStatusSavedAPI;');
