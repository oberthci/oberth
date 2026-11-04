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
  assert.ok(ids().indexOf('push-head') < ids().indexOf('scheduled-running'),'a queued head precedes running work fetched from the active view');
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
if (!process.env.DASHBOARD_RUNS_CASE || process.env.DASHBOARD_RUNS_CASE === 'F3') {
  // Queued runs accepted before several runs started and finished must never
  // render among the finished rows, whatever order the page or the active
  // view delivers them in: queue (next admission lowest), running, finished.
  run(`
    state.filter='all';flatPaging=false;changeRunRepo('all');state.runCursor=0;state.runCursorHistory=[];
    const f3Time=minutes=>new Date(Date.UTC(2026,10,3,10,minutes)).toISOString();
    const f3Page=[
      makeRun('passed-newest',{Ref:'r1',StartedAt:f3Time(0),FinishedAt:f3Time(50)}),
      makeRun('passed-older',{Ref:'r2',StartedAt:f3Time(5),FinishedAt:f3Time(40)}),
      makeRun('queued-normal-late',{Ref:'r3',Status:'queued',StartedAt:null,FinishedAt:null,QueuedAt:f3Time(20),QueuePosition:3}),
      makeRun('queued-high-next',{Ref:'r4',Status:'queued',StartedAt:null,FinishedAt:null,QueuedAt:f3Time(21),QueuePosition:1,priority_class:'high'}),
      makeRun('failed-middle',{Ref:'r5',Status:'failed',StartedAt:f3Time(10),FinishedAt:f3Time(45)}),
      makeRun('running-early',{Ref:'r6',Status:'running',StartedAt:f3Time(30),FinishedAt:null}),
    ];
    const f3Active=[
      makeRun('running-late-hidden',{Ref:'r7',Status:'running',StartedAt:f3Time(35),FinishedAt:null}),
      makeRun('waiting-hidden',{Ref:'r9',Status:'running',StartedAt:null,FinishedAt:null,QueuedAt:f3Time(23),QueuePosition:4,display_status:'queued'}),
      makeRun('queued-high-second-hidden',{Ref:'r8',Status:'queued',StartedAt:null,FinishedAt:null,QueuedAt:f3Time(22),QueuePosition:2,priority_class:'high'}),
    ];
    api=async path=>{
      if(path==='/api/repos')return [{ID:1,Name:'ours',UpstreamID:1}];
      if(path==='/api/runs/counts')return {Total:9,ByRepo:{1:9}};
      const query=new URL(path,'https://example.test').searchParams;
      if(query.get('view')==='active')return [...f3Active,...f3Page.filter(row=>['queued','running'].includes(row.Status))];
      return {body:f3Page,next:''};
    };
  `);
  await run('renderRuns(0)');
  assert.deepEqual(ids(), ['queued-high-next','queued-high-second-hidden','queued-normal-late','waiting-hidden','running-late-hidden','running-early','passed-newest','failed-middle','passed-older'],
    'F3: queue in claim order by position, then running newest start first, then finished newest finish first');
  assert.ok(!run('runPageFull(f3Page)'), 'F3: queued and running rows never count toward the terminal page limit');
  // A scoped viewer sees no queue positions: the active view's order (the
  // server's admission order) decides, never the queue time, so an older
  // normal run still sits below a newer high run that is admitted next.
  run(`
    const f3Scoped=[
      makeRun('scoped-passed',{Ref:'s1',StartedAt:f3Time(0),FinishedAt:f3Time(50)}),
      makeRun('scoped-normal-old',{Ref:'s2',Status:'queued',StartedAt:null,FinishedAt:null,QueuedAt:f3Time(20)}),
      makeRun('scoped-high-new',{Ref:'s3',Status:'queued',StartedAt:null,FinishedAt:null,QueuedAt:f3Time(25),priority_class:'high'}),
    ];
    const f3ScopedHidden=makeRun('scoped-normal-hidden',{Ref:'s4',Status:'queued',StartedAt:null,FinishedAt:null,QueuedAt:f3Time(22)});
    api=async path=>{
      if(path==='/api/repos')return [{ID:1,Name:'ours',UpstreamID:1}];
      if(path==='/api/runs/counts')return {Total:4,ByRepo:{1:4}};
      const query=new URL(path,'https://example.test').searchParams;
      if(query.get('view')==='active')return [f3Scoped[2],f3Scoped[1],f3ScopedHidden];
      return {body:f3Scoped,next:''};
    };
  `);
  await run('renderRuns(0)');
  assert.deepEqual(ids(), ['scoped-high-new','scoped-normal-old','scoped-normal-hidden','scoped-passed'],
    'F3: without positions the queue follows the active view order, not queue time');
  run(`api=async path=>{ if(path==='/api/repos')return [{ID:1,Name:'ours',UpstreamID:1}]; if(path==='/api/runs/counts')return {Total:3,ByRepo:{1:3}}; const query=new URL(path,'https://example.test').searchParams; if(query.get('view')==='active')return []; return {body:f3Scoped,next:''}; };`);
  await run('renderRuns(0)');
  assert.deepEqual(ids(), ['scoped-high-new','scoped-normal-old','scoped-passed'],
    'F3: without positions or an active view priority leads, preserving server FIFO within each class');
  // A claimed normal run (raw running, not started) listed by the active
  // view after an unclaimed high run stays behind it: the scoped queue is
  // the server's claim order, never "claimed first".
  run(`
    const f3Claimed=[
      makeRun('scoped-claimed-normal',{Ref:'s5',Status:'running',StartedAt:null,FinishedAt:null,QueuedAt:f3Time(20),display_status:'queued',display_reason:'waiting for CPU budget (2 of 8 CPU free)'}),
      makeRun('scoped-high-unclaimed',{Ref:'s6',Status:'queued',StartedAt:null,FinishedAt:null,QueuedAt:f3Time(25),priority_class:'high',display_status:'queued',display_reason:'next in line'}),
      makeRun('scoped-started',{Ref:'s7',Status:'running',StartedAt:f3Time(30),FinishedAt:null,display_status:'running'}),
    ];
    api=async path=>{
      if(path==='/api/repos')return [{ID:1,Name:'ours',UpstreamID:1}];
      if(path==='/api/runs/counts')return {Total:3,ByRepo:{1:3}};
      const query=new URL(path,'https://example.test').searchParams;
      if(query.get('view')==='active')return [f3Claimed[2],f3Claimed[1],f3Claimed[0]];
      return {body:f3Claimed,next:''};
    };
  `);
  await run('renderRuns(0)');
  assert.deepEqual(ids(), ['scoped-high-unclaimed','scoped-claimed-normal','scoped-started'],
    'F3: a claimed normal run sits below the unclaimed high run the gate admits first; both are queued rows');
  assert.ok(!/waiting<\/span>/.test(run('html')), 'F3: the claimed run is a queued pill, not waiting');
  run('api=apiFixture;');
  console.log('F3 passed: queued runs never render below running or finished runs');
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
assert.ok(!badgeRow.includes('class="v4b') && !badgeRow.includes('nightly') && !badgeRow.includes('deploy'),'list/history rows have no inline badges');
assert.ok(badgeRow.includes('priority highest') && badgeRow.includes('urgent'),'priority is visible in list/history messages');
assert.ok(run(`prioritySummary({priority_class:'high',priority_reason:'urgent',priority_requested_by:'operator<unsafe>',QueuePosition:2})`).includes('requested by operator<unsafe>'));
assert.ok(run(`runRow({ID:'priority',RepoID:1,Ref:'main',Status:'queued',priority_class:'high',priority_requested_by:'operator<unsafe>',QueuePosition:2})`).includes('operator&lt;unsafe&gt;'),'requester metadata is escaped');
assert.equal(run(`runMessageCell({Status:'passed',Release:true})`),'release');
assert.equal(run(`runMessageCell({Status:'passed',Trigger:'schedule',ScheduleEntry:'nightly<unsafe>'})`),'schedule: nightly&lt;unsafe&gt;');
assert.equal(run(`runMessageCell({Status:'passed',priority_class:'highest',ConcurrencyGroup:'deploy'})`),'priority highest');
for(const extra of [`display_reason:'existing reason'`, `Error:'existing reason'`, `FailedStep:'test'`, `Reason:'existing reason'`]) {
  assert.equal(run(`runMessageCell({Status:'failed',Release:true,Trigger:'schedule',ScheduleEntry:'nightly',${extra}})`), extra.startsWith('FailedStep')?'failed at test':'existing reason');
}
assert.equal(run(`runMessageCell({Status:'queued',Release:true,Trigger:'schedule',ScheduleEntry:'nightly',display_reason:'concurrency group deploy busy (held by run 0123456789ab)',Scheduling:{state:'waiting',admission:{group_holder:true,group:'deploy'}}})`),'concurrency group deploy busy (held by run 0123456789ab)');
assert.equal(run(`runMessageCell({Status:'queued',Scheduling:{state:'queued',message:'Awaiting scheduler claim.'}})`),'Awaiting scheduler claim.','the scheduling message is the fallback reason');
assert.ok(run(`releaseBadge({Release:true,priority_class:'highest',priority_reason:'urgent'})`).includes('highest priority'),'detail priority and reason remain available');
console.log('R4B passed: label-free rows, empty-message fallbacks, existing reasons and detail badges');

// One grey queued status for every run that has not started. The raw wire
// status "waiting" and the scheduling states (preparing, waiting, admitted,
// preflight) are reasons, never a pill of their own.
if (!process.env.DASHBOARD_RUNS_CASE || process.env.DASHBOARD_RUNS_CASE === 'V1') {
  const pills = html => [...html.matchAll(/<span class="pill ([a-z]+)"><span class="dot [a-z]+"><\/span>([^<]*)<\/span>/g)].map(m => [m[1], m[2]]);
  const notStarted = [
    {ID:'claimed', Status:'running', StartedAt:null, display_status:'queued', display_reason:'waiting for CPU budget (12.5 of 40 CPU free)', Scheduling:{state:'waiting', admission:{state:'waiting'}}},
    {ID:'wire-waiting', Status:'waiting', StartedAt:null, display_status:'queued', display_reason:'host IO pressure (42%)', Scheduling:{state:'waiting'}},
    {ID:'admitted', Status:'running', StartedAt:null, display_status:'queued', display_reason:'admitted, starting the workflow', Scheduling:{state:'waiting', admission:{state:'admitted'}}},
    {ID:'preflight', Status:'running', StartedAt:null, display_status:'queued', display_reason:'KVM preflight running', Scheduling:{state:'preflight'}},
    {ID:'durable', Status:'queued', StartedAt:null, display_status:'queued', display_reason:'next in line', Scheduling:{state:'queued', queue_position:1}},
    {ID:'raw-only', Status:'waiting', StartedAt:null},
  ];
  sandbox.notStarted = notStarted;
  const table = run('runsTable(notStarted)');
  for (const [kind, label] of pills(table)) assert.deepEqual([kind, label], ['pending', 'queued'], 'V1: every not-started run is one grey queued pill');
  assert.ok(!/waiting<\/span>/.test(table) && !table.includes('>admitted<') && !table.includes('>preflight<'), 'V1: no second not-started status');
  for (const row of notStarted) if (row.display_reason) assert.ok(table.includes(row.display_reason), 'V1: the reason line shows ' + row.display_reason);
  for (const row of notStarted) assert.equal(run(`runListGroup(notStarted.find(r => r.ID === '${row.ID}'))`), 0, 'V1: ' + row.ID + ' sorts in the queue group');
  // The vocabulary is seven statuses; each has a known kind and label.
  for (const status of ['queued', 'running', 'passed', 'failed', 'infra-retried', 'superseded', 'cancelled']) {
    assert.notEqual(run(`statusKind('${status}')`), 'unknown', 'V1: ' + status + ' has a kind');
  }
  for (const gone of ['waiting', 'infra-pending', 'deadline-exceeded', 'interrupted']) {
    assert.equal(run(`statusKind('${gone}')`), 'unknown', 'V1: ' + gone + ' is not a run status');
  }
  assert.equal(run(`displayRunStatus({Status:'failed', display_status:'failed', display_reason:'Execution deadline exceeded.'})`), 'failed');
  assert.equal(run(`displayRunStatus({Status:'running', StartedAt:'2026-11-03T10:00:00Z', display_status:'running', Phase:'infrastructure-pending'})`), 'running');
  console.log('V1 passed: one queued status, reasons on the message line');
}

// Every list view preserves effective priority, including claimed work and
// aged normal work, even when a scoped viewer has no global queue positions.
run('var claimListPath="";');
for (const latest of [false, true]) {
  for (const scoped of [false, true]) {
    sandbox.claimOrderScoped = scoped;
    sandbox.claimOrderLatest = latest;
    run(`{
      changeRunRepo('all'); state.filter=claimOrderLatest?'all':'queued'; state.activeRuns=[];
      claimListPath='';
      const claimOrderRows=[
        makeRun('claim-high-first',{Ref:'q1',Status:'queued',StartedAt:null,FinishedAt:null,QueuePosition:1,priority_class:'high'}),
        makeRun('claim-aged',{Ref:'q2',Status:'queued',StartedAt:null,FinishedAt:null,QueuePosition:2,priority_class:'high',priority_reason:'aged: normal run waited at least 60 minutes'}),
        makeRun('claim-high-last',{Ref:'q3',Status:'queued',StartedAt:null,FinishedAt:null,QueuePosition:3,priority_class:'high'}),
        makeRun('claim-normal',{Ref:'q4',Status:'waiting',StartedAt:null,FinishedAt:null,QueuePosition:4,display_status:'queued'}),
      ].map(row=>claimOrderScoped?{...row,QueuePosition:0}:row);
      api=async path=>{
        if(path==='/api/repos')return [{ID:1,Name:'ours',UpstreamID:1}];
        if(path==='/api/runs/counts')return {Total:4,ByRepo:{1:4}};
        if(path.includes('view=active'))return claimOrderRows;
        claimListPath=path;
        return {body:claimOrderLatest?[claimOrderRows[2],claimOrderRows[0]]:claimOrderRows,next:''};
      };
    }`);
    await run('renderRuns(state.routeSeq)');
    assert.deepEqual(ids(), ['claim-high-first','claim-aged','claim-high-last','claim-normal'], `claim order: latest=${latest}, scoped=${scoped}`);
    assert.equal(new URL(run('claimListPath'),'https://example.test').searchParams.has('latest_per_ref'), latest, 'exercise both flat and latest-per-ref requests');
  }
}
console.log('Queued groups preserve scheduler priority and FIFO in flat, latest-per-ref and scoped views');

// A scoped poll can receive a newer priority bump in the heads while the
// active request fails and retains its older queue order. Cached order must
// never put a normal row between two priority rows.
run(`{
  changeRunRepo('all'); state.filter='all';
  const high=makeRun('cache-high',{Ref:'cache/high',Status:'queued',StartedAt:null,FinishedAt:null,priority_class:'high'});
  const normal=makeRun('cache-normal',{Ref:'cache/normal',Status:'queued',StartedAt:null,FinishedAt:null});
  const bumped=makeRun('cache-bumped',{Ref:'cache/bumped',Status:'queued',StartedAt:null,FinishedAt:null,priority_class:'highest',priority_reason:'operator bump'});
  state.activeRuns=[high,normal,{...bumped,priority_class:'normal'}];
  state.activeRunsRepo='all'; state.activeRunsComplete=true;
  api=async path=>{
    if(path==='/api/repos')return [{ID:1,Name:'ours',UpstreamID:1}];
    if(path==='/api/runs/counts')return {Total:3,ByRepo:{1:3}};
    if(path.includes('view=active'))throw Object.assign(new Error('offline'),{status:503});
    return {body:[bumped,normal],next:''};
  };
}`);
await run('renderRuns(state.routeSeq)');
assert.deepEqual(ids(), ['cache-bumped','cache-high','cache-normal'], 'a stale scoped active order must not interleave effective priority classes');
console.log('Priority classes remain grouped when the scoped active request fails after a priority change');
