// Execute the shipped dashboard functions with deterministic browser/API mocks.
import { readFileSync } from 'node:fs';
import vm from 'node:vm';
import assert from 'node:assert/strict';
const source = readFileSync(new URL('../internal/api/static/app.js', import.meta.url), 'utf8');
const sandbox = vm.createContext({ console, URLSearchParams, Map, Set, Date, location:{pathname:'/runs/live-run'}, document: {getElementById(){return null;}}, localStorage:{getItem(){return null;}}, AbortController, setTimeout() {}, clearTimeout() {}, setInterval() {}, clearInterval() {}, requestAnimationFrame() {return 1;}, cancelAnimationFrame() {} });
vm.runInContext(source.slice(0, source.indexOf('/* ---------- events ---------- */')), sandbox);
const run = code => vm.runInContext(code, sandbox);
run(`
  repoName = () => 'oberth';
  const fixtureRun = { id: 'live-run', ref: 'fix/live', phase: 'test', status: 'running' };
  // Drive fallback ticks explicitly; metadata rendering no longer polls logs.
  const realLiveLogContent = liveLogContent;
  liveLogContent = async (...args) => {
    if (state.live) await pollLiveLog(state.live);
    const html = await realLiveLogContent(...args);
    return html + liveDeliverySummary(state.live, args[1], args[2]);
  };
  let polls = [];
  api = async () => { const next = polls.shift(); if (next instanceof Error) throw next; return next; };
`);
assert.equal(run(`JSON.stringify(selectedLiveLines(['[build/one] one', '[build/two] wrong', '[build/one-extra] wrong', '[build/one] repeat', '[build/one] repeat'], 'build', 'one'))`), JSON.stringify(['one', 'repeat', 'repeat']));
run(`polls.push({chunk:'[build/one] fir', offset:15}); awaitResult = liveLogContent(fixtureRun, 'build', 'one');`);
await run('awaitResult');
assert.equal(run('state.live.text'), '');
run(`polls.push({chunk:'st\\n[build/two] foreign\\n::oberth-log:: {"burn":"build","step":"one","state":"following"}\\n', offset:150}); awaitResult = liveLogContent(fixtureRun, 'build', 'one');`);
let html = await run('awaitResult');
assert.ok(html.includes('first'));
assert.ok(!html.includes('foreign'));
assert.ok(!html.includes('::oberth-log::'));
assert.ok(html.includes('following'));
assert.ok(html.includes('delivery unconfirmed'));
run(`state.live.delivery.get('build/one/').at = new Date().toISOString();`);
assert.ok(run(`liveDeliverySummary(state.live, 'build', 'one')`).includes('pod delivery: following'));
assert.ok(run(`liveDeliverySummary(state.live, 'build', 'one', Date.now()+11000)`).includes('delivery unconfirmed'));
run(`polls.push(new Error('offline')); awaitResult = liveLogContent(fixtureRun, 'build', 'one');`);
html = await run('awaitResult');
assert.ok(html.includes('reconnecting: offline'));
assert.ok(html.includes('first'));
assert.equal(run('state.live.offset'), 150);
run(`polls.push({chunk:'', offset:150}); awaitResult = liveLogContent(fixtureRun, 'build', 'one');`);
html = await run('awaitResult');
assert.ok(html.includes('polling connected'));
assert.ok(!html.includes('offline'));
assert.ok(run(`liveDeliverySummary(state.live, 'build', 'one', Date.now() + 60000)`).includes('quiet period'));
// Scope raw records before interpreting carriage returns as visual lines.
// A sibling's progress text must not impersonate a selected-step prefix.
run(`polls.push({chunk:'[build/one] progress-1\\rprogress-2\\n[build/two] sibling\\r[build/one] foreign-progress\\n', offset:250}); awaitResult = liveLogContent(fixtureRun, 'build', 'one');`);
html = await run('awaitResult');
assert.ok(html.includes('progress-2'), 'carriage-return output stays visible');
assert.ok(!html.includes('foreign-progress'), 'carriage returns cannot change attribution');
assert.ok(!html.includes('[build/one]'), 'selected logs omit the retained attribution prefix');
run(`polls.push({chunk:'[build/one] final-without-newline', offset:280, done:true}); awaitResult = liveLogContent(fixtureRun, 'build', 'one');`);
html = await run('awaitResult');
assert.ok(html.includes('final-without-newline'), 'the final poll flushes an unterminated tail');
assert.equal(run('state.live.pending'), '');
assert.equal(run('state.live.done'), true);
// Empty final polls must flush a fragment retained by the preceding poll too.
run(`state.live.done = false; polls.push({chunk:'[build/one] split-final', offset:300}); awaitResult = liveLogContent(fixtureRun, 'build', 'one');`);
await run('awaitResult');
assert.ok(!run('state.live.text').includes('split-final'));
run(`polls.push({chunk:'', offset:300, done:true}); awaitResult = liveLogContent(fixtureRun, 'build', 'one');`);
assert.ok((await run('awaitResult')).includes('split-final'));
// Retained single-step logs remove only the outer transport attribution,
// leaving command-authored bracketed messages untouched.
run(`polls.push({output:'[release-build/full-our-step-name] actual log\\n[release-build/full-our-step-name] [INFO] user message\\n'}); awaitResult = stepLogContent('completed', {burn:'release-build', step:'full-our-step-name', status:'passed'}, false);`);
html = await run('awaitResult');
assert.ok(html.includes('actual log') && html.includes('[INFO] user message'));
assert.ok(!html.includes('[release-build/full-our-step-name]'));
html = run(`renderLogLines(['[build/one] actual one', '[build/two] actual two', '[build/one] actual next'], {groupSteps:true})`);
assert.ok(!html.includes('[build/one]') && !html.includes('[build/two]'));
assert.equal((html.match(/log-hdr/g) || []).length, 3, 'combined output labels each change of step');
html = run(`renderLogLines(splitAttributedLogText('[build/two] progress\\r[build/one] command text\\n'), {groupSteps:true})`);
assert.equal((html.match(/log-hdr/g) || []).length, 1, 'CR updates cannot forge combined-view step headers');
assert.ok(html.includes('── build / two ──'));

// API/plan order need not be execution order. Equal-time parallel steps use a
// stable ordinal tie-breaker; pending steps remain after all started steps.
run(`const orderedFixture = [
  {burn:'ci', step:'pending', ordinal:0, status:'pending'},
  {burn:'ci', step:'late', ordinal:1, started_at:'2026-10-23T01:00:02Z'},
  {burn:'ci', step:'parallel-b', ordinal:3, started_at:'2026-10-23T01:00:01Z'},
  {burn:'ci', step:'early', ordinal:99, started_at:'2026-10-23T01:00:00Z'},
  {burn:'ci', step:'parallel-a', ordinal:2, started_at:'2026-10-23T01:00:01Z'}
];`);
assert.equal(run(`orderSteps(orderedFixture).map(row => row.step.step).join(',')`), 'early,parallel-a,parallel-b,late,pending');
run(`const legacyRetryFixture = [
  {burn:'build(2)', step:'compile', ordinal:3, started_at:'2026-10-23T01:00:03Z'},
  {burn:'lint', step:'lint', ordinal:1, started_at:'2026-10-23T01:00:01Z'},
  {burn:'build', step:'compile', ordinal:0, started_at:'2026-10-23T01:00:00Z'},
  {burn:'build(1)', step:'compile', ordinal:2, started_at:'2026-10-23T01:00:02Z'}
];`);
assert.equal(run(`orderSteps(legacyRetryFixture).map(row => row.step.burn).join(',')`), 'build,build(1),build(2),lint');
// #1196: run 02609902 at 22:38Z, before vpt-verify starts. Argo start
// times have one-second resolution; parallel siblings must not split burns.
run(`const burnOrderFixture = [
  {"burn":"setup","step":"tools-dir","ordinal":0,"status":"passed","started_at":"2026-10-23T22:32:46Z"},
  {"burn":"setup","step":"link-go","ordinal":1,"status":"passed","started_at":"2026-10-23T22:33:06Z"},
  {"burn":"setup","step":"install-golangci-lint","ordinal":2,"status":"passed","started_at":"2026-10-23T22:33:16Z"},
  {"burn":"setup","step":"install-helm","ordinal":3,"status":"passed","started_at":"2026-10-23T22:33:36Z"},
  {"burn":"setup","step":"fetch-shellcheck","ordinal":4,"status":"passed","started_at":"2026-10-23T22:33:56Z"},
  {"burn":"setup","step":"verify-shellcheck","ordinal":5,"status":"passed","started_at":"2026-10-23T22:34:06Z"},
  {"burn":"setup","step":"decompress-shellcheck","ordinal":6,"status":"passed","started_at":"2026-10-23T22:34:17Z"},
  {"burn":"setup","step":"install-shellcheck","ordinal":7,"status":"passed","started_at":"2026-10-23T22:34:27Z"},
  {"burn":"lint","step":"vet","ordinal":8,"status":"passed","started_at":"2026-10-23T22:34:37Z"},
  {"burn":"lint","step":"golangci-lint-run","ordinal":9,"status":"passed","started_at":"2026-10-23T22:35:07Z"},
  {"burn":"test-store","step":"test-store","ordinal":10,"status":"running","started_at":"2026-10-23T22:37:32Z"},
  {"burn":"test-service","step":"test-service","ordinal":11,"status":"running","started_at":"2026-10-23T22:37:32Z"},
  {"burn":"test-rest","step":"test-rest","ordinal":12,"status":"running","started_at":"2026-10-23T22:37:32Z"},
  {"burn":"test-openbao","step":"test-openbao","ordinal":13,"status":"running","started_at":"2026-10-23T22:34:37Z"},
  {"burn":"security","step":"security","ordinal":14,"status":"passed","started_at":"2026-10-23T22:34:37Z"},
  {"burn":"shellcheck","step":"shellcheck","ordinal":15,"status":"passed","started_at":"2026-10-23T22:34:37Z"},
  {"burn":"chart","step":"chart","ordinal":16,"status":"passed","started_at":"2026-10-23T22:37:32Z"},
  {"burn":"release-diagnostic","step":"release-diagnostic","ordinal":17,"status":"passed","started_at":"2026-10-23T22:34:37Z"},
  {"burn":"verify-publisher-tools","step":"vpt-build-and-fetch","ordinal":18,"status":"running","started_at":"2026-10-23T22:34:37Z"},
  {"burn":"verify-publisher-tools","step":"vpt-copy-trivy","ordinal":19,"status":"passed","started_at":"2026-10-23T22:34:37Z"},
  {"burn":"verify-publisher-tools","step":"vpt-verify","ordinal":20,"status":"pending","started_at":""},
  {"burn":"build-amd64","step":"build-amd64","ordinal":21,"status":"pending","started_at":""},
  {"burn":"build-arm64","step":"build-arm64","ordinal":22,"status":"pending","started_at":""},
  {"burn":"build-darwin-amd64","step":"build-darwin-amd64","ordinal":23,"status":"pending","started_at":""},
  {"burn":"build-darwin-arm64","step":"build-darwin-arm64","ordinal":24,"status":"pending","started_at":""},
];`);
assert.equal(run(`burnOrderFixture.length`), 25);
const expectedBurnOrder = [
  'tools-dir', 'link-go', 'install-golangci-lint', 'install-helm',
  'fetch-shellcheck', 'verify-shellcheck', 'decompress-shellcheck', 'install-shellcheck',
  'vet', 'golangci-lint-run', 'test-openbao', 'security', 'shellcheck',
  'release-diagnostic', 'vpt-build-and-fetch', 'vpt-copy-trivy', 'vpt-verify',
  'test-store', 'test-service', 'test-rest', 'chart',
  'build-amd64', 'build-arm64', 'build-darwin-amd64', 'build-darwin-arm64'
].join(',');
assert.equal(run(`orderSteps(burnOrderFixture).map(row => row.step.step).join(',')`), expectedBurnOrder);
// Finishing a step cannot move it or its burn across polls.
assert.equal(run(`orderSteps(burnOrderFixture.map(step => step.status === 'running' ? {...step, status:'passed', finished_at:'2026-10-23T22:38:01Z'} : step)).map(row => row.step.step).join(',')`), expectedBurnOrder);
// Starting the last sibling of an active burn keeps it inside that block.
assert.equal(run(`orderSteps(burnOrderFixture.map(step => step.step === 'vpt-verify' ? {...step, status:'running', started_at:'2026-10-23T22:38:52Z'} : step)).map(row => row.step.step).join(',')`), expectedBurnOrder);
// Reversing the API rows does not change unique plan-ordinal tie breaks.
assert.equal(run(`orderSteps([...burnOrderFixture].reverse()).map(row => row.step.step).join(',')`), expectedBurnOrder);
assert.equal(run(`orderSteps([]).length`), 0);
assert.equal(run(`orderSteps(null).length`), 0);

// Equal burn ordinals still form contiguous blocks; invalid starts are pending.
run(`const tiedBurnFixture = [
  {burn:'second', step:'a', ordinal:1, started_at:'invalid'},
  {burn:'first', step:'original', ordinal:0},
  {burn:'third', step:'a', ordinal:1},
  {burn:'first', step:'original(1)', ordinal:0},
  {burn:'second', step:'b', ordinal:2},
  {burn:'third', step:'b', ordinal:2},
  {burn:'first', step:'last', ordinal:0}
];`);
assert.equal(run(`orderSteps(tiedBurnFixture).map(row => row.label).join(',')`),
  'first/original,first/last,first/original,second/a,second/b,third/a,third/b');
// A newly started burn moves ahead of wholly unstarted burns.
assert.equal(run(`orderSteps(tiedBurnFixture.map(step => step.burn === 'third' && step.step === 'a' ? {...step, started_at:'2026-10-23T22:38:00Z'} : step)).map(row => row.identity.burn).join(',')`),
  'third,third,first,first,first,second,second');

// An oversized partial record keeps its original attribution. Trimming its
// head would make its continuation look like a fresh record from another step.
run(`state.live.text = ''; ingestLiveChunk(state.live, '[build/two] ' + 'x'.repeat(200000-12) + '[build/one] forged' + 'x'.repeat(300000-18), Date.now());`);
run(`ingestLiveChunk(state.live, ' remainder\\n[build/one] real\\n', Date.now());`);
assert.equal(run(`selectedLiveLines(state.live.text.split('\\n'), 'build', 'one').join('\\n')`), 'real');
assert.ok(run('state.live.text').includes('[truncated]'));
run(`ingestLiveChunk(state.live, '[build/one] ' + 'x'.repeat(500000) + '\\n', Date.now());`);
assert.ok(run('state.live.text.length') <= 400000);
assert.ok(run('state.live.pending.length') <= 400000);
run(`ingestLiveChunk(state.live, ('[build/one] small\\n').repeat(10000), Date.now());`);
assert.equal(run(`(renderLogLines(splitLogText(state.live.text)).match(/class="log-line/g) || []).length`), 5001);

// Exercise the actual detail renderer's scroll restoration and combined-view
// selection, including a terminal poll that must continue draining chunks.
run(`
  state.repos = [{id: 1, name:'oberth'}];
  state.stepRunID = 'scroll-run'; state.step = '__summary__';
  currentRoute = () => true; setChrome = () => {}; replaceApp = () => {};
  liveLogContent = async () => 'fixture log';
  const oldTerm = {scrollHeight:1000, scrollTop:123, clientHeight:100};
  const oldSteps = {scrollTop:45};
  const newTerm = {scrollHeight:1200, scrollTop:0, clientHeight:100};
  const newSteps = {scrollTop:0};
  let termReads = 0, stepReads = 0;
  document = {getElementById(id) {
    if(id==='termBody') return termReads++ === 0 ? oldTerm : newTerm;
    if(id==='stepList') return stepReads++ === 0 ? oldSteps : newSteps;
    return null;
  }};
  setAuto = () => {};
  awaitResult = renderRunDetailView({run:{id:'scroll-run', repo_id:1, status:'running', ref:'fix/live'}, steps:[]}, 0);
`);
await run('awaitResult');
assert.equal(run('newTerm.scrollTop'), 123);
assert.equal(run('newSteps.scrollTop'), 45);
run(`termReads=0; stepReads=0; oldTerm.scrollTop=900; awaitResult=renderRunDetailView({run:{id:'scroll-run',repo_id:1,status:'running',started_at:new Date().toISOString(),ref:'fix/live'},steps:[]},0);`);
await run('awaitResult');
assert.equal(run('newTerm.scrollTop'), 1200);

run(`
  let renderedHTML = '';
  replaceApp = html => { renderedHTML = html; };
  state.step = 'build / compile';
  state.stepRunID = 'scroll-run';
  termReads=0; stepReads=0;
  awaitResult=renderRunDetailView({run:{id:'scroll-run',repo_id:1,status:'running',started_at:new Date().toISOString(),ref:'fix/live'},steps:[{burn:'build',step:'compile',status:'queued',ordinal:0}]},0);
`);
await run('awaitResult');
assert.ok(run('renderedHTML').includes('fixture log'), 'a queued pod can emit before Argo publishes Running');

// Empty row messages identify schedules, escaped; detail keeps its schedule badge.
run(`const scheduleRun = {id:'scroll-run', repo_id:1, status:'passed', ref:'main', trigger:'schedule', schedule_entry:'nightly<unsafe>', display_reason:'schedule nightly<unsafe>'};`);
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

// Run-list vocabulary is additive: raw failure/cancellation states survive.
assert.equal(run(`displayRunStatus({status:'failed'})`), 'failed');
assert.equal(run(`displayRunStatus({status:'interrupted',superseded_by:'new'})`), 'superseded');
assert.equal(run(`displayRunStatus({status:'interrupted'})`), 'cancelled');
// The list always shows branch/tag heads, with history only on run detail.
assert.equal(run(`state.filter='all';runMatches({status:'interrupted',superseded_by:'new'})`), true);
run(`state.attempts.clear();state.attempts.set('head',{rows:[{id:'old',repo_id:1,status:'interrupted',superseded_by:'head'}],next:''});`);
const headTable = run(`runsTable([{id:'head',repo_id:1,status:'passed',ref:'feature',earlier_attempts:1}])`);
assert.ok(!headTable.includes('earlier attempts') && !headTable.includes('data-run-id="old"'));
assert.ok(run(`runHistory({id:'head',ref_kind:'branch'})`).includes('data-run-id="old"'), 'superseded runs remain available in detail history');
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
 state.detailRun={id:'rca-head',repo_id:1,ref:'feature',ref_kind:'branch',status:'passed'};
 api=async path=>{rcaRequests.push(path);return {body:[state.detailRun,{id:'our-old',repo_id:1,ref:'feature',ref_kind:'branch',status:'interrupted',superseded_by:'rca-head'},{id:'tag-same-name',repo_id:1,ref:'feature',ref_kind:'tag'},{id:'other-repo',repo_id:2,ref:'feature',ref_kind:'branch'},{id:'other-ref',repo_id:1,ref:'elsewhere',ref_kind:'branch'}],next:'opaque-history-cursor'};};
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

assert.equal(run(`summaryLines({run:{id:'original',status:'failed',display_reason:'infrastructure error'},steps:[]}).some(line => line.startsWith('[queued]'))`), false);

// Controller-profile holds must be visible and refresh without delaying logs.
assert.equal(run(`controllerProfileStatusCard(null)`), '');
const pausedProfile = `{state:'blocked',admission_paused:true,reason:'ReplicaSet <not-ready>',remedy:'Restore one Ready replacement'}`;
const profileCard = run(`controllerProfileStatusCard(${pausedProfile})`);
const pauseBanner = run(`admissionPausedBanner(${pausedProfile})`);
for (const text of ['admission paused','ReplicaSet &lt;not-ready&gt;','Restore one Ready replacement']) assert.ok(profileCard.includes(text), text);
assert.ok(pauseBanner.includes('Admission paused') && pauseBanner.includes('ReplicaSet &lt;not-ready&gt;'));
assert.equal(run(`admissionPausedBanner({state:'verified',admission_paused:false})`), '');
assert.ok(run(`admissionPausedBanner({state:'unverified'})`).includes('Admission paused'));
run(`
 const capacityStatusSavedAPI = api;
 let profileStatusCalls = 0;
 state.status = {version:'test',controller_profile:${pausedProfile}};
 state.statusLoadedAt = Date.now(); state.statusFailedAt = 0;
 api = async () => { profileStatusCalls++; return {version:'test',controller_profile:{state:'verified',admission_paused:false}}; };
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
assert.equal(run('admissionPausedBanner(state.status.controller_profile)'), '', 'recovery clears cached admission banner');
run(`statusRefreshPromise=loadStatus();`);
await run('statusRefreshPromise');
assert.equal(run('profileStatusCalls'), 2, 'visiting Status must fetch fresh data during the background cooldown');
run(`state.statusLoadedAt=Date.now()-300001; api=async()=>{profileStatusCalls++;throw new Error('status unavailable')}; statusRefreshPromise=ensureStatus();`);
await run('statusRefreshPromise');
run('statusRefreshPromise=ensureStatus();');
await run('statusRefreshPromise');
assert.equal(run('profileStatusCalls'), 3, 'status failures retain the 30s retry backoff');
run('api=capacityStatusSavedAPI;');
