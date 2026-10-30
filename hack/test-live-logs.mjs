// Execute the shipped dashboard functions with deterministic browser/API mocks.
import { readFileSync } from 'node:fs';
import vm from 'node:vm';
import assert from 'node:assert/strict';
const source = readFileSync(new URL('../internal/api/static/app.js', import.meta.url), 'utf8');
const sandbox = vm.createContext({ console, URLSearchParams, Map, Set, Date, location:{pathname:'/runs/live-run'}, document: {getElementById(){return null;}}, localStorage:{getItem(){return null;}}, setInterval() {}, clearInterval() {}, requestAnimationFrame() {return 1;}, cancelAnimationFrame() {} });
vm.runInContext(source.slice(0, source.indexOf('/* ---------- events ---------- */')), sandbox);
const run = code => vm.runInContext(code, sandbox);
run(`
  repoName = () => 'oberth';
  const fixtureRun = { ID: 'live-run', Ref: 'fix/live', Phase: 'test', Status: 'running' };
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
run(`polls.push({output:'[release-build/full-our-step-name] actual log\\n[release-build/full-our-step-name] [INFO] user message\\n'}); awaitResult = stepLogContent('completed', {Burn:'release-build', Step:'full-our-step-name', Status:'passed'}, false);`);
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
  {Burn:'ci', Step:'pending', Ordinal:0, Status:'pending'},
  {Burn:'ci', Step:'late', Ordinal:1, StartedAt:'2026-10-23T01:00:02Z'},
  {Burn:'ci', Step:'parallel-b', Ordinal:3, StartedAt:'2026-10-23T01:00:01Z'},
  {Burn:'ci', Step:'early', Ordinal:99, StartedAt:'2026-10-23T01:00:00Z'},
  {Burn:'ci', Step:'parallel-a', Ordinal:2, StartedAt:'2026-10-23T01:00:01Z'}
];`);
assert.equal(run(`orderSteps(orderedFixture).map(row => row.step.Step).join(',')`), 'early,parallel-a,parallel-b,late,pending');
run(`const legacyRetryFixture = [
  {Burn:'build(2)', Step:'compile', Ordinal:3, StartedAt:'2026-10-23T01:00:03Z'},
  {Burn:'lint', Step:'lint', Ordinal:1, StartedAt:'2026-10-23T01:00:01Z'},
  {Burn:'build', Step:'compile', Ordinal:0, StartedAt:'2026-10-23T01:00:00Z'},
  {Burn:'build(1)', Step:'compile', Ordinal:2, StartedAt:'2026-10-23T01:00:02Z'}
];`);
assert.equal(run(`orderSteps(legacyRetryFixture).map(row => row.step.Burn).join(',')`), 'build,build(1),build(2),lint');
// #1196: run 02609902 at 22:38Z, before vpt-verify starts. Argo start
// times have one-second resolution; parallel siblings must not split burns.
run(`const burnOrderFixture = [
  {"Burn":"setup","Step":"tools-dir","Ordinal":0,"Status":"passed","StartedAt":"2026-10-23T22:32:46Z"},
  {"Burn":"setup","Step":"link-go","Ordinal":1,"Status":"passed","StartedAt":"2026-10-23T22:33:06Z"},
  {"Burn":"setup","Step":"install-golangci-lint","Ordinal":2,"Status":"passed","StartedAt":"2026-10-23T22:33:16Z"},
  {"Burn":"setup","Step":"install-helm","Ordinal":3,"Status":"passed","StartedAt":"2026-10-23T22:33:36Z"},
  {"Burn":"setup","Step":"fetch-shellcheck","Ordinal":4,"Status":"passed","StartedAt":"2026-10-23T22:33:56Z"},
  {"Burn":"setup","Step":"verify-shellcheck","Ordinal":5,"Status":"passed","StartedAt":"2026-10-23T22:34:06Z"},
  {"Burn":"setup","Step":"decompress-shellcheck","Ordinal":6,"Status":"passed","StartedAt":"2026-10-23T22:34:17Z"},
  {"Burn":"setup","Step":"install-shellcheck","Ordinal":7,"Status":"passed","StartedAt":"2026-10-23T22:34:27Z"},
  {"Burn":"lint","Step":"vet","Ordinal":8,"Status":"passed","StartedAt":"2026-10-23T22:34:37Z"},
  {"Burn":"lint","Step":"golangci-lint-run","Ordinal":9,"Status":"passed","StartedAt":"2026-10-23T22:35:07Z"},
  {"Burn":"test-store","Step":"test-store","Ordinal":10,"Status":"running","StartedAt":"2026-10-23T22:37:32Z"},
  {"Burn":"test-service","Step":"test-service","Ordinal":11,"Status":"running","StartedAt":"2026-10-23T22:37:32Z"},
  {"Burn":"test-rest","Step":"test-rest","Ordinal":12,"Status":"running","StartedAt":"2026-10-23T22:37:32Z"},
  {"Burn":"test-openbao","Step":"test-openbao","Ordinal":13,"Status":"running","StartedAt":"2026-10-23T22:34:37Z"},
  {"Burn":"security","Step":"security","Ordinal":14,"Status":"passed","StartedAt":"2026-10-23T22:34:37Z"},
  {"Burn":"shellcheck","Step":"shellcheck","Ordinal":15,"Status":"passed","StartedAt":"2026-10-23T22:34:37Z"},
  {"Burn":"chart","Step":"chart","Ordinal":16,"Status":"passed","StartedAt":"2026-10-23T22:37:32Z"},
  {"Burn":"release-diagnostic","Step":"release-diagnostic","Ordinal":17,"Status":"passed","StartedAt":"2026-10-23T22:34:37Z"},
  {"Burn":"verify-publisher-tools","Step":"vpt-build-and-fetch","Ordinal":18,"Status":"running","StartedAt":"2026-10-23T22:34:37Z"},
  {"Burn":"verify-publisher-tools","Step":"vpt-copy-trivy","Ordinal":19,"Status":"passed","StartedAt":"2026-10-23T22:34:37Z"},
  {"Burn":"verify-publisher-tools","Step":"vpt-verify","Ordinal":20,"Status":"pending","StartedAt":""},
  {"Burn":"build-amd64","Step":"build-amd64","Ordinal":21,"Status":"pending","StartedAt":""},
  {"Burn":"build-arm64","Step":"build-arm64","Ordinal":22,"Status":"pending","StartedAt":""},
  {"Burn":"build-darwin-amd64","Step":"build-darwin-amd64","Ordinal":23,"Status":"pending","StartedAt":""},
  {"Burn":"build-darwin-arm64","Step":"build-darwin-arm64","Ordinal":24,"Status":"pending","StartedAt":""},
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
assert.equal(run(`orderSteps(burnOrderFixture).map(row => row.step.Step).join(',')`), expectedBurnOrder);
// Finishing a step cannot move it or its burn across polls.
assert.equal(run(`orderSteps(burnOrderFixture.map(step => step.Status === 'running' ? {...step, Status:'passed', FinishedAt:'2026-10-23T22:38:01Z'} : step)).map(row => row.step.Step).join(',')`), expectedBurnOrder);
// Starting the last sibling of an active burn keeps it inside that block.
assert.equal(run(`orderSteps(burnOrderFixture.map(step => step.Step === 'vpt-verify' ? {...step, Status:'running', StartedAt:'2026-10-23T22:38:52Z'} : step)).map(row => row.step.Step).join(',')`), expectedBurnOrder);
// Reversing the API rows does not change unique plan-ordinal tie breaks.
assert.equal(run(`orderSteps([...burnOrderFixture].reverse()).map(row => row.step.Step).join(',')`), expectedBurnOrder);
assert.equal(run(`orderSteps([]).length`), 0);
assert.equal(run(`orderSteps(null).length`), 0);

// Equal burn ordinals still form contiguous blocks; invalid starts are pending.
run(`const tiedBurnFixture = [
  {Burn:'second', Step:'a', Ordinal:1, StartedAt:'invalid'},
  {Burn:'first', Step:'original', Ordinal:0},
  {Burn:'third', Step:'a', Ordinal:1},
  {Burn:'first', Step:'original(1)', Ordinal:0},
  {Burn:'second', Step:'b', Ordinal:2},
  {Burn:'third', Step:'b', Ordinal:2},
  {Burn:'first', Step:'last', Ordinal:0}
];`);
assert.equal(run(`orderSteps(tiedBurnFixture).map(row => row.label).join(',')`),
  'first/original,first/last,first/original,second/a,second/b,third/a,third/b');
// A newly started burn moves ahead of wholly unstarted burns.
assert.equal(run(`orderSteps(tiedBurnFixture.map(step => step.Burn === 'third' && step.Step === 'a' ? {...step, StartedAt:'2026-10-23T22:38:00Z'} : step)).map(row => row.identity.burn).join(',')`),
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
  state.repos = [{ID: 1, Name:'oberth'}];
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
  awaitResult = renderRunDetailView({Run:{ID:'scroll-run', RepoID:1, Status:'running', Ref:'fix/live'}, Steps:[]}, 0);
`);
await run('awaitResult');
assert.equal(run('newTerm.scrollTop'), 123);
assert.equal(run('newSteps.scrollTop'), 45);
run(`termReads=0; stepReads=0; oldTerm.scrollTop=900; awaitResult=renderRunDetailView({Run:{ID:'scroll-run',RepoID:1,Status:'running',StartedAt:new Date().toISOString(),Ref:'fix/live'},Steps:[]},0);`);
await run('awaitResult');
assert.equal(run('newTerm.scrollTop'), 1200);

run(`
  let renderedHTML = '';
  replaceApp = html => { renderedHTML = html; };
  state.step = 'build / compile';
  state.stepRunID = 'scroll-run';
  termReads=0; stepReads=0;
  awaitResult=renderRunDetailView({Run:{ID:'scroll-run',RepoID:1,Status:'running',StartedAt:new Date().toISOString(),Ref:'fix/live'},Steps:[{Burn:'build',Step:'compile',Status:'queued',Ordinal:0}]},0);
`);
await run('awaitResult');
assert.ok(run('renderedHTML').includes('fixture log'), 'a queued pod can emit before Argo publishes Running');

// Schedule entries distinguish otherwise identical runs and remain escaped.
run(`const scheduleRun = {ID:'scroll-run', RepoID:1, Status:'passed', Ref:'main', Trigger:'schedule', ScheduleEntry:'nightly<unsafe>'};`);
html = run(`runsTable([scheduleRun, {...scheduleRun, ID:'weekly-run', ScheduleEntry:'weekly-sweep'}])`);
assert.ok(html.includes('nightly&lt;unsafe&gt;') && html.includes('weekly-sweep'));
assert.ok(!html.includes('nightly<unsafe>'));
assert.equal(run(`scheduleBadge({...scheduleRun, Trigger:'branch'})`), '');
assert.equal(run(`scheduleBadge({...scheduleRun, ScheduleEntry:''})`), '');
run(`state.step='__summary__'; termReads=0; stepReads=0; awaitResult=renderRunDetailView({Run:scheduleRun,Steps:[]},0);`);
await run('awaitResult');
assert.ok(run('renderedHTML').includes('title="schedule entry">nightly&lt;unsafe&gt;'));
assert.ok(run(`summaryLines({Run:scheduleRun,Steps:[]}).join(' ')`).includes('schedule=nightly<unsafe>'));

assert.equal(run(`statusKind('deadline-exceeded')`), 'fail');
const deadlineSummary = run(`summaryLines({Run:{ID:'deadline',Status:'running',display_status:'deadline-exceeded'},Steps:[],Scheduling:{message:'Execution deadline exceeded.',execution:{state:'deadline_exceeded',phase:'Failed',reconciliation_lag_seconds:3080,deadline:{budget_seconds:5400,expires_at:'2026-10-30T10:33:37Z',remaining_seconds:0}}}}).join(' ')`);
for (const text of ['deadline-exceeded', 'Execution deadline exceeded', '3080s', '0s remaining', 'clock: Workflow start']) assert.ok(deadlineSummary.includes(text), text);
assert.ok(!deadlineSummary.includes('[queued]'));
run(`state.step='build / compile'; state.stepRunID='scroll-run'; termReads=0; stepReads=0; awaitResult=renderRunDetailView({Run:{ID:'scroll-run',RepoID:1,Status:'running',display_status:'deadline-exceeded',StartedAt:new Date().toISOString(),Ref:'fix/live'},Steps:[{Burn:'build',Step:'compile',Status:'running',Ordinal:0}]},0);`);
await run('awaitResult');
assert.ok(run('renderedHTML').includes('fixture log') && run('renderedHTML').includes('live output'), 'an execution display label must not stop durable-running log refresh');
console.log('live log dashboard checks passed');

// Host pressure is a scheduling hold, distinct from running-work failures.
assert.equal(run(`queueReason({Scheduling:{state:'waiting',admission:{state:'waiting',pressure:{resource:'io',avg60:42.25}}}})`), 'deferred: io pressure 42.3%');
assert.equal(run(`queueReason({Scheduling:{state:'waiting',admission:{state:'waiting',blocked_by:{pressure:{resource:'cpu',avg60:75},size:'M'}}}})`), 'deferred: cpu pressure 75.0%');
assert.equal(run(`pressureStatusCard(null)`), '', 'scoped status omits host PSI');
assert.ok(run(`pressureStatusCard({available:false})`).includes('admission fails open'));
assert.ok(run(`pressureStatusCard({available:true,holding:true,resource:'io',sample:{io:{full_avg60:40},cpu:{some_avg60:20},memory:{full_avg60:5}}})`).includes('deferring new grants'));
