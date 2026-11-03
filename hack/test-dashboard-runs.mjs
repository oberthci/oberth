// Run-list regressions against the shipped script, with deterministic API data.
import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import vm from 'node:vm';
const source = readFileSync(process.env.DASHBOARD_SCRIPT || new URL('../internal/api/static/app.js', import.meta.url), 'utf8');
const sandbox = vm.createContext({URLSearchParams, console, Map, Set, Date, location:{pathname:'/runs'}, document:{body:{dataset:{version:'test'}},getElementById(){return null}}, localStorage:{getItem(){return null},setItem(){}}, setInterval(){},clearInterval(){}});
vm.runInContext(source.slice(0, source.indexOf('/* ---------- events ---------- */')), sandbox);
const run = code => vm.runInContext(code, sandbox);
const numericCapacity = run(`capacityStatusCard({configuration:{max_concurrent_jobs:4,source:'config'},used:0,capacity:8,numeric_budget:{cpu_used_milli:7000,cpu_budget_milli:8000,memory_used_bytes:1073741824,memory_budget_bytes:17179869184}})`);
assert.match(numericCapacity, /CPU 7000m\/8000m/, 'capacity card must expose numeric CPU budget even when weight use is zero');
assert.match(numericCapacity, /memory/, 'capacity card must expose the memory budget');
run(`
  let html='', requests=[], listNext='', flatPaging=false;
  replaceApp=value=>{html=value};
  const makeRun=(ID,extra={})=>{
    const row={ID,RepoID:1,Ref:'main',RefKind:'branch',Status:'passed',StartedAt:'2026-11-01T10:00:00Z',FinishedAt:'2026-11-01T10:01:00Z',...extra};
    if(['running','queued'].includes(row.Status)) {
      row.priority_class ??= 'normal'; row.priority_reason ??= 'ordinary run';
      row.display_reason ??= 'Priority '+row.priority_class+': '+row.priority_reason+'.';
    }
    return row;
  };
  const head=makeRun('push-head',{Status:'queued',StartedAt:null,FinishedAt:null});
  const scheduled=makeRun('scheduled-running',{Status:'running',Trigger:'schedule',ScheduleEntry:'nightly',FinishedAt:null});
  const queued=makeRun('publication-queued',{Status:'queued',Ref:'other',StartedAt:null,FinishedAt:null});
  const failed=makeRun('nightly-failed',{Status:'failed',Trigger:'schedule',ScheduleEntry:'nightly'});
  const superseded=makeRun('superseded-old',{Status:'interrupted',SupersededBy:head.ID});
  const retried=makeRun('retried-old',{Ref:'feature',Status:'failed',auto_retried_by:'retry-head'});
  state.status={upstream_info:[{id:1,name:'github',base_url:'https://example.test/org'}]};state.statusLoadedAt=Date.now();
  api=async path=>{
    requests.push(path);
    if(path==='/api/repos')return [{ID:1,Name:'ours',UpstreamID:1},{ID:2,Name:'theirs',UpstreamID:1}];
    if(path==='/api/runs/counts')return {Total:300,ByRepo:{1:299,2:1}};
    const query=new URL(path,'https://example.test').searchParams;
    if(query.get('view')==='active')return [scheduled,queued,head,...(query.has('repo')?[]:[makeRun('foreign-running',{RepoID:2,Status:'running',FinishedAt:null})])];
    let rows;
    if(query.has('latest_per_ref'))rows=query.has('before')?[makeRun('page-two-head',{Ref:'old-ref'})]:[head,makeRun('retry-head',{Ref:'feature'})];
    else if(flatPaging)rows=[head,...Array.from({length:100},(_,i)=>makeRun('flat-'+i,{Ref:'ref-'+i}))];
    else rows=[head,scheduled,queued,failed,superseded,retried,makeRun('retry-head',{Ref:'feature'})];
    return {body:rows,next:listNext};
  };
  const apiFixture=api;
`);
// URL is supplied separately so the script's browser globals stay minimal.
sandbox.URL = URL;
const ids = () => [...run('html').matchAll(/class="trow" data-run-id="([^"]+)"/g)].map(match=>match[1]);
if (!process.env.DASHBOARD_RUNS_CASE || process.env.DASHBOARD_RUNS_CASE === 'F1') {
  await run('renderRuns(0)');
  assert.ok(ids().includes('scheduled-running'), 'F1: scheduled work hidden by a newer push must remain visible');
  assert.ok(ids().includes('publication-queued'), 'F1: a queued run outside the heads must remain visible');
  assert.equal(ids().filter(id=>id==='push-head').length,1,'active heads must not duplicate');
  assert.ok(ids().indexOf('scheduled-running') < ids().indexOf('push-head'),'missing active runs precede heads');
  run(`state.runCursor='page-two';state.runCursorHistory=[0];requests=[];`);
  await run('renderRuns(0)');
  assert.ok(ids().includes('page-two-head') && ids().includes('scheduled-running') && ids().includes('push-head'),'F1: page two must include all active work, even heads from page one');
  run(`changeRunRepo('github/org/ours');state.runCursor='page-two';requests=[];`);
  await run('renderRuns(0)');
  assert.ok(!ids().includes('foreign-running'),'active rows obey repository scope');
  assert.ok(run(`requests.some(path=>path.includes('view=active')&&path.includes('repo=github%2Forg%2Fours')&&!path.includes('before='))`),'active fetch stays unpaged and repository-scoped');
  console.log('F1 passed: hidden scheduled/queued work, deduplication, page two and repository scope');
}
if (!process.env.DASHBOARD_RUNS_CASE || process.env.DASHBOARD_RUNS_CASE === 'F2') {
  run(`changeRunRepo('all');`);
  for(const [status,id] of [['failed','nightly-failed'],['superseded','superseded-old'],['infra-retried','retried-old']]) {
    run(`state.filter=${JSON.stringify(status)};requests=[];`);
    await run('renderRuns(0)');
    assert.ok(ids().includes(id),`F2: ${status} must find a non-head run, including another schedule lane`);
    assert.ok(run(`requests.some(path=>path.startsWith('/api/runs?')&&!path.includes('view=active')&&!path.includes('latest_per_ref'))`),'F2: status filters use the flat list');
  }
  run(`state.filter='passed';flatPaging=true;listNext='opaque-flat-cursor';`);
  await run('renderRuns(0)');
  assert.equal(run('state.runNext'),'opaque-flat-cursor','flat paging counts dated rows, not its extra queued rows');
  console.log('F2 passed: failed nightly, superseded and infra-retried filters; flat-list queue pagination');
}
if (!process.env.DASHBOARD_RUNS_CASE || process.env.DASHBOARD_RUNS_CASE === 'R4') {
  run(`
    state.filter='all';flatPaging=false;changeRunRepo('all');
    let pollMs=0, activeFailure=null;
    const workingAPI=api;
    setAuto=ms=>{pollMs=ms};hasToken=()=>true;setConn=()=>{};
    api=async path=>{
      const query=new URL(path,'https://example.test').searchParams;
      if(query.get('view')==='active' && activeFailure)throw activeFailure;
      return workingAPI(path);
    };
  `);
  await run('route(true)');
  for (const status of [503, undefined]) {
    run(`activeFailure=Object.assign(new Error('active fetch unavailable'),{status:${JSON.stringify(status)}});pollMs=0;`);
    await run('route(true)');
    assert.ok(ids().includes('push-head'), 'R4: active fetch failure must keep the run table');
    assert.ok(ids().includes('scheduled-running'), 'R4: transient failure retains the last active rows');
    assert.equal(run('pollMs'),4000,'R4: active fetch failure must not stop polling');
    assert.ok(!run('html').includes('all active runs included'),'R4: unavailable active data must not be advertised as complete');
    assert.ok(run('html').includes('active runs unavailable'));
  }
  run(`changeRunRepo('github/org/ours');`);
  await run('route(true)');
  assert.ok(!ids().includes('foreign-running'),'retained active rows still obey the selected repository');
  run(`changeRunRepo('all');`);
  for (const status of [400,404,409]) {
    run(`activeFailure=Object.assign(new Error('invalid repository'),{status:${status}});pollMs=0;`);
    await run('route(true)');
    assert.ok(ids().includes('push-head'));
    assert.equal(run('state.activeRuns.length'),0,'invalid selectors clear the active list');
    assert.equal(run('pollMs'),4000);
  }
  // Cold load with a removed qualified selector and status still unavailable.
  run(`
    state.status=null;ensureStatus=()=>Promise.resolve();changeRunRepo('github/org/removed');
    api=async path=>{
      if(new URL(path,'https://example.test').searchParams.get('repo')==='github/org/removed')throw Object.assign(new Error('repository not found'),{status:404});
      return workingAPI(path);
    };pollMs=0;
  `);
  await run('route(true)');
  assert.equal(run('state.repo'),'all');
  assert.ok(ids().includes('push-head'),'stale saved repository falls back to the all-repository table');
  assert.equal(run('pollMs'),4000);
  run(`api=workingAPI;activeFailure=null;pollMs=0;`);
  await run('route(true)');
  assert.ok(ids().includes('scheduled-running'));
  assert.ok(run('html').includes('all active runs included'),'a successful poll clears the unavailable notice');
  assert.ok(run('html').includes('Retained runs'),'touch users have a visible explanation of the sidebar counts');
  run(`
    state.detailRun={ID:'history-full-id',RepoID:1,Ref:'main',RefKind:'branch'};
    state.attempts.set('history-full-id',{rows:[],next:'older-cursor'});
    let finishRefresh, olderCalls=0;
    api=async path=>{
      if(new URL(path,'https://example.test').searchParams.has('before')){olderCalls++;return {body:[],next:''};}
      return new Promise(resolve=>{finishRefresh=resolve});
    };
    pendingRefresh=loadRunAttempts('history-full-id',false,true);
  `);
  await run(`loadRunAttempts('history-full-id',true)`);
  assert.equal(run('olderCalls'),0,'older click waits for the in-flight refresh');
  run(`finishRefresh({body:[],next:'older-cursor'})`);
  await run('pendingRefresh');
  assert.equal(run('olderCalls'),1,'queued older click runs after refresh');
  console.log('R4 passed: active 503/network/400/404/409 failures, stale saved repository, polling and recovery');
}

if (!process.env.DASHBOARD_RUNS_CASE || process.env.DASHBOARD_RUNS_CASE === 'R5A') {
  for (const [extra, expected] of [[{Trigger:'schedule',ScheduleEntry:'nightly'},'schedule: nightly'],[{Release:true},'release'],[{},''],[{Error:'execution failed'},'execution failed'],[{display_reason:'KVM preflight running'},'KVM preflight running']]) {
    sandbox.rowExtra=extra;
    run(`rowFixture=makeRun('row-message',{Status:'running',FinishedAt:null,...rowExtra})`);
    assert.equal(run('runMessageCell(rowFixture)'),expected,'R5A: priority-only display reason must not replace the row message fallback');
    const tooltip=[...run('runRow(rowFixture)').matchAll(/<div class="clip" title="([^"]*)">/g)].at(-1)[1];
    assert.equal(tooltip,expected,'R5A: tooltip uses the same priority suppression and fallback');
  }
  assert.equal(run(`runMessage(makeRun('detail',{Status:'running'}))`),'Priority normal: ordinary run.','detail presentation keeps the original priority reason');
  console.log('R5A passed: real active priority projection, row/tooltip fallbacks and preserved execution reasons');
}
if (!process.env.DASHBOARD_RUNS_CASE || process.env.DASHBOARD_RUNS_CASE === 'R5B') {
  run(`
    api=apiFixture;state.status=null;state.statusPending=null;ensureStatus=()=>Promise.resolve();
    state.filter='all';flatPaging=false;changeRunRepo('github/org/ours');state.activeRuns=[];
    let r5PollMs=0;setAuto=ms=>{r5PollMs=ms};hasToken=()=>true;setConn=()=>{};
  `);
  await run('route(true)');
  assert.ok(ids().includes('scheduled-running'),'R5B: a successful scoped active fetch must stay visible before status arrives');
  assert.ok(run('html').includes('all active runs included'));
  run(`api=async path=>{if(path.includes('view=active'))throw Object.assign(new Error('offline'),{status:503});return apiFixture(path)};`);
  await run('route(true)');
  assert.ok(ids().includes('scheduled-running'),'same-selector cached rows remain usable without status');
  run(`changeRunRepo('github/org/theirs');`);
  await run('route(true)');
  assert.ok(!ids().includes('scheduled-running'),'failed fetch must not retain active rows from another selector');
  assert.equal(run('state.activeRuns.length'),0);
  assert.ok(!run('html').includes('all active runs included'));
  assert.equal(run('r5PollMs'),4000);
  console.log('R5B passed: cold valid saved repo, missing status, same-scope retention and cross-scope failure');
}

// The shared row renderer serves /runs and run history; detail badges stay separate.
run(`state.repos=[{ID:1,Name:'ours'}];`);
const badgeRow = run(`runRow({ID:'labels',RepoID:1,Ref:'v1',RefKind:'tag',Status:'passed',Release:true,Trigger:'schedule',ScheduleEntry:'nightly',priority_class:'highest',priority_reason:'urgent',ConcurrencyGroup:'deploy',Scheduling:{admission:{group:'deploy'}}})`);
assert.ok(badgeRow.includes('<div class="repo-name">ours</div>'));
assert.ok(!badgeRow.includes('class="v4b') && !badgeRow.includes('priority') && !badgeRow.includes('nightly') && !badgeRow.includes('deploy'),'list/history repo cell has no inline labels');
assert.equal(run(`runMessageCell({Status:'passed',Release:true})`),'release');
assert.equal(run(`runMessageCell({Status:'passed',Trigger:'schedule',ScheduleEntry:'nightly<unsafe>'})`),'schedule: nightly&lt;unsafe&gt;');
assert.equal(run(`runMessageCell({Status:'passed',priority_class:'highest',ConcurrencyGroup:'deploy'})`),'');
for(const extra of [`display_reason:'existing reason'`, `Error:'existing reason'`, `FailedStep:'test'`, `Reason:'existing reason'`]) {
  assert.equal(run(`runMessageCell({Status:'failed',Release:true,Trigger:'schedule',ScheduleEntry:'nightly',${extra}})`), extra.startsWith('FailedStep')?'failed at test':'existing reason');
}
assert.equal(run(`runMessageCell({Status:'queued',Release:true,Trigger:'schedule',ScheduleEntry:'nightly',Scheduling:{state:'waiting',admission:{group_holder:true,group:'deploy'}}})`),'waiting for group deploy');
assert.ok(run(`releaseBadge({Release:true,priority_class:'highest',priority_reason:'urgent'})`).includes('highest priority'),'detail priority and reason remain available');
console.log('R4B passed: label-free rows, empty-message fallbacks, existing reasons and detail badges');
