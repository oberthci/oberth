// Run-list regressions against the shipped script, with deterministic API data.
import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import vm from 'node:vm';
const source = readFileSync(process.env.DASHBOARD_SCRIPT || new URL('../internal/api/static/app.js', import.meta.url), 'utf8');
const sandbox = vm.createContext({URLSearchParams, console, Map, Set, Date, location:{pathname:'/runs'}, document:{body:{dataset:{version:'test'}},getElementById(){return null}}, localStorage:{getItem(){return null},setItem(){}}, setInterval(){},clearInterval(){}});
vm.runInContext(source.slice(0, source.indexOf('/* ---------- events ---------- */')), sandbox);
const run = code => vm.runInContext(code, sandbox);
run(`
  let html='', requests=[], listNext='', flatPaging=false;
  replaceApp=value=>{html=value};
  const makeRun=(ID,extra={})=>({ID,RepoID:1,Ref:'main',RefKind:'branch',Status:'passed',StartedAt:'2026-11-01T10:00:00Z',FinishedAt:'2026-11-01T10:01:00Z',...extra});
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
