// Optional rendered-page regression, using synthetic read-only API responses.
// PLAYWRIGHT_MODULE=/path/to/playwright/index.mjs PLAYWRIGHT_CHANNEL=chrome \
// DASHBOARD_EVIDENCE_DIR=private scratch node hack/test-dashboard-runs-browser.mjs
import assert from 'node:assert/strict';
import {readFileSync, mkdirSync, writeFileSync} from 'node:fs';
import {createServer} from 'node:http';
const {chromium} = await import(process.env.PLAYWRIGHT_MODULE || 'playwright');
const root = new URL('../', import.meta.url);
const read = path => readFileSync(new URL(path, root));
const evidence = process.env.DASHBOARD_EVIDENCE_DIR;
if (evidence) mkdirSync(evidence, {recursive: true});
const serverSource = read('internal/api/server.go').toString();
const shell = serverSource.match(/io.WriteString\(writer, `(<\!doctype html>.*?)`\)/s)[1]
  .replaceAll('`+versionedAssetURL("app.css")+`', '/assets/app.css')
  .replaceAll('`+versionedAssetURL("app.js")+`', '/assets/app.js')
  .replaceAll('`+html.EscapeString(server.version)+`', 'test-fixture')
  .replaceAll('`+view+`', 'runs');
const now = Date.now();
const makeRun = (ID, extra = {}) => {
  const row = {ID, RepoID: 1, Ref: 'main', RefKind: 'branch', Status: 'passed', SHA: 'abc123456789', Actor: 'fixture', Trigger: 'push', QueuedAt: new Date(now-90000).toISOString(), StartedAt: new Date(now-80000).toISOString(), FinishedAt: new Date(now-20000).toISOString(), ...extra};
  if (['running','queued'].includes(row.Status)) {
    row.priority_class ??= 'normal'; row.priority_reason ??= 'ordinary run';
    row.display_reason ??= `Priority ${row.priority_class}: ${row.priority_reason}.`;
  }
  return row;
};
const heads = [makeRun('head-main', {earlier_attempts: 3, priority_class:'highest', priority_reason:'fixture priority', ConcurrencyGroup:'deploy'}), makeRun('head-tag', {Ref:'v1', RefKind:'tag', Release:true, Credentialed:true}), makeRun('head-branch', {Ref:'v1', Status:'failed'}), makeRun('head-queued', {RepoID:2, Status:'queued', StartedAt:null, FinishedAt:null}), makeRun('head-retry', {Ref:'feature', Status:'queued', StartedAt:null, FinishedAt:null, auto_retry_of:'retry-failed'})];
const older = makeRun('old-failed', {Status:'failed', Error:'fixture failure', Trigger:'schedule', ScheduleEntry:'weekly', priority_class:'high', priority_reason:'history priority', ConcurrencyGroup:'deploy'});
const superseded = makeRun('old-superseded', {Status:'interrupted', SupersededBy:'head-main'});
const oldest = makeRun('oldest');
const scheduled = makeRun('scheduled-running', {Status:'running', Trigger:'schedule', ScheduleEntry:'nightly', FinishedAt:null});
const scheduledQueued = makeRun('scheduled-queued', {Status:'queued', Trigger:'schedule', ScheduleEntry:'daily', StartedAt:null, FinishedAt:null});
const nightlyFailed = makeRun('nightly-failed', {Status:'failed', Trigger:'schedule', ScheduleEntry:'nightly', Error:'nightly failed before a newer successful push'});
const retried = makeRun('retry-failed', {Ref:'feature', Status:'failed', auto_retried_by:'head-retry'});
const active = [scheduled, scheduledQueued, heads[3], heads[4]];
const flat = [...heads, older, superseded, oldest, nightlyFailed, retried, scheduled, scheduledQueued];
const repos = [{ID:1, Name:'oberth', UpstreamID:1}, {ID:2, Name:'example', UpstreamID:1}, {ID:3, Name:'work-laptop-identities', UpstreamID:1}];
const status = {version:'test-fixture', upstream_info:[{id:1,name:'github',base_url:'https://example.test/org'}]};
const requests = [];
let baseline = false, paging = false, historyError = false, sharedNames = false, delayStatus = false, releaseStatus, activeError = 0, delayOlder = false, releaseOlder;
const server = createServer((req, res) => {
  const url = new URL(req.url, 'http://localhost');
  requests.push(url.pathname + url.search);
  const json = (body, next) => {res.writeHead(200, {'Content-Type':'application/json', ...(next ? {'X-Oberth-Next-Before':next} : {})}); res.end(JSON.stringify(body));};
  if (url.pathname.startsWith('/api/')) assert.equal(req.headers.authorization, 'Bearer fixture-token');
  if (url.pathname === '/api/repos') return json(sharedNames ? [repos[0], {ID:4,Name:'oberth',UpstreamID:2}] : repos);
  if (url.pathname === '/api/status') {
    const value = sharedNames ? {...status, upstream_info:[...status.upstream_info, {id:2,name:'codeberg',base_url:'https://example.test/team'}]} : status;
    if (delayStatus) {releaseStatus = () => json(value); return;}
    return json(value);
  }
  if (url.pathname === '/api/runs/counts') return json({Total:12, ByRepo:{1:11,2:1,3:0}});
  if (url.pathname === '/api/runs') {
    if (url.searchParams.get('view') === 'active') {
      assert.ok(!url.searchParams.has('before') && !url.searchParams.has('latest_per_ref'));
      if (activeError) {res.writeHead(activeError, {'Content-Type':'application/json'}); return res.end(JSON.stringify({error:'active list temporarily unavailable'}));}
      return json(active.filter(run => !url.searchParams.has('repo') || `github/org/${repos.find(repo=>repo.ID===run.RepoID)?.Name}` === url.searchParams.get('repo')));
    }
    if (url.searchParams.has('ref')) {
      if (sharedNames && !url.searchParams.get('repo')?.includes('/')) {res.writeHead(409, {'Content-Type':'application/json'}); return res.end(JSON.stringify({error:'ambiguous repository'}));}
      if (historyError) {res.writeHead(503, {'Content-Type':'application/json'}); return res.end(JSON.stringify({error:'fixture history unavailable'}));}
      if (url.searchParams.has('before')) {
        if (delayOlder) {releaseOlder=()=>json([older, oldest]); return;}
        return json([older, oldest]);
      }
      return json([heads[0], older, superseded, makeRun('same-name-tag',{RefKind:'tag'}), makeRun('other-repo',{RepoID:2}), makeRun('other-ref',{Ref:'elsewhere'})], 'opaque-history-cursor');
    }
    if (paging) {
      if (url.searchParams.has('before')) return json([makeRun('page-two', {Ref:'old-branch'})]);
      return json(Array.from({length:100}, (_,i)=>makeRun(`page-one-${i}`, {Ref:`branch-${i}`})), 'opaque-list-cursor');
    }
    let rows = url.searchParams.has('latest_per_ref') ? heads : flat;
    if (url.searchParams.has('repo')) rows = rows.filter(run => run.RepoID === repos.find(repo => `github/org/${repo.Name}` === url.searchParams.get('repo'))?.ID);
    return json(rows);
  }
  if (url.pathname.startsWith('/api/runs/')) {
    const id = decodeURIComponent(url.pathname.split('/')[3]);
    const Run = flat.find(run => run.ID === id || (id.length >= 8 && run.ID.startsWith(id)));
    return json({Run, Steps:[{Burn:'ci',Step:'publish',Ordinal:2,Status:'passed',Secrets:true,StartedAt:new Date(now-40000).toISOString(),FinishedAt:new Date(now-20000).toISOString()}, {Burn:'ci',Step:'test',Ordinal:1,Status:'passed',StartedAt:new Date(now-60000).toISOString(),FinishedAt:new Date(now-40000).toISOString()}]});
  }
  if (url.pathname === '/assets/app.js' || url.pathname === '/assets/app.css') {
    const name = url.pathname.split('/').pop();
    res.writeHead(200, {'Content-Type':name.endsWith('.js') ? 'text/javascript' : 'text/css'});
    return res.end(baseline ? readFileSync(`${process.env.DASHBOARD_BASELINE_DIR}/before-${name}`) : read(`internal/api/static/${name}`));
  }
  if (url.pathname.startsWith('/assets/fonts/')) {res.writeHead(200, {'Content-Type':'font/woff2'}); return res.end(read(`internal/api/static/fonts/${url.pathname.split('/').pop()}`));}
  res.writeHead(200, {'Content-Type':'text/html'}); res.end(shell);
});
await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
let browser;
try {
  browser = await chromium.launch({headless:true, ...(process.env.PLAYWRIGHT_CHANNEL ? {channel:process.env.PLAYWRIGHT_CHANNEL} : {})});
  const origin = `http://127.0.0.1:${server.address().port}`;
  const results = [];
  for (const width of [1440, 1100, 390, 320]) {
    const context = await browser.newContext({viewport:{width,height:900}});
    const page = await context.newPage();
    page.setDefaultTimeout(5000);
    const errors = [];
    page.on('pageerror', error => errors.push(error.message));
    await page.addInitScript(() => {
      localStorage.setItem('oberth-token','fixture-token');
      // Old saved options must not bring historical rows back to /runs.
      localStorage.setItem('oberth-collapse-runs','false');
      localStorage.setItem('oberth-show-superseded','false');
    });
    const capture = async name => {
      if (!evidence) return;
      await page.evaluate(() => document.fonts.ready);
      await page.screenshot({path:`${evidence}/${name}-${width}.png`, fullPage:true});
      writeFileSync(`${evidence}/${name}-${width}.html`, await page.content());
    };
    if (process.env.DASHBOARD_BASELINE_DIR && [1440,390].includes(width)) {
      baseline = true;
      await page.goto(`${origin}/runs`);
      await page.locator('.runs-table').waitFor();
      await capture('before-runs');
      await page.goto(`${origin}/runs/head-main`);
      await page.locator('[data-detail-run]').waitFor();
      await capture('before-detail');
    }
    baseline = false;
    await page.goto(`${origin}/runs`);
    await page.locator('.runs-table').waitFor();
    assert.equal(await page.locator('.runs-table > [data-run-id]').count(), heads.length + 2);
    assert.equal(await page.locator('[data-action="toggle-attempts"],[data-action="toggle-collapse-runs"],[data-action="toggle-superseded"]').count(), 0);
    assert.equal(await page.locator('[data-repo-filter="github/org/work-laptop-identities"] .count').textContent(), '0');
    assert.equal(await page.locator('[data-repo-filter="github/org/work-laptop-identities"] .count').getAttribute('aria-label'), '0 retained runs');
    assert.equal(await page.locator('[data-repo-filter="github/org/example"] .count').getAttribute('title'), '1 retained run');
    assert.equal(await page.locator('[data-repo-filter="github/org/work-laptop-identities"] .name').evaluate(el=>el.scrollWidth<=el.clientWidth), true, 'long repository name is fully visible');
    assert.equal(await page.locator('.runs-table > [data-run-id="scheduled-running"]').count(), 1);
    assert.equal(await page.locator('.runs-table > [data-run-id="head-queued"]').count(), 1);
    assert.ok(!(await page.locator('.runs-toolbar').textContent()).includes('Latest run'));
    assert.ok((await page.locator('.runs-footer').textContent()).includes('all active runs included'));
    const layout = await page.evaluate(() => ({side:document.querySelector('.repo-side').getBoundingClientRect().top,table:document.querySelector('.runs-table').getBoundingClientRect().top,screen:document.querySelector('.screen').getBoundingClientRect().width,viewport:innerWidth,sideRight:document.querySelector('.repo-side').getBoundingClientRect().right,tableRight:document.querySelector('.runs-table').getBoundingClientRect().right,filterRight:Math.max(...[...document.querySelectorAll('.runs-toolbar button')].map(button=>button.getBoundingClientRect().right))}));
    if (width > 980) assert.equal(layout.side, layout.table, `sidebar/table alignment at ${width}`);
    assert.ok(layout.screen <= width && layout.sideRight <= width && layout.tableRight <= width && layout.filterRight <= width, `list, repository counts and every filter fit ${width}px: ${JSON.stringify(layout)}`);
    assert.equal(await page.locator('.runs-table .repo-name .v4b').count(),0,'list rows have no inline badges');
    assert.equal(await page.locator('.runs-table > [data-run-id="head-tag"] > div:last-child').textContent(),'release');
    assert.equal(await page.locator('.runs-table > [data-run-id="scheduled-running"] > div:last-child').textContent(),'schedule: nightly');
    assert.equal(await page.locator('.runs-table > [data-run-id="scheduled-running"] > div:last-child').getAttribute('title'),'schedule: nightly');
    assert.ok(!(await page.locator('.runs-table > .trow > div:last-child').allTextContents()).some(text=>text.startsWith('Priority ')));
    assert.equal(await page.locator('.runs-table > [data-run-id="scheduled-queued"] > div:last-child').textContent(),'awaiting scheduler','a queued fixture without an admission observation uses the scheduler fallback');
    assert.equal(await page.locator('.repo-side .side-head').textContent(),'Retained runs');
    await capture('after-runs');
    if (width === 1440) {
      activeError=503;
      await page.evaluate(()=>route(true));
      assert.equal(await page.locator('.runs-table > [data-run-id="scheduled-running"]').count(),1);
      assert.ok((await page.locator('.runs-footer').textContent()).includes('active runs unavailable'));
      assert.ok(await page.evaluate(()=>!!state.timer),'polling stays enabled after active fetch failure');
      await capture('after-active-unavailable');
      activeError=0;
      await page.evaluate(()=>route(true));
      assert.ok((await page.locator('.runs-footer').textContent()).includes('all active runs included'));
    }
    for (const [filter, id] of [['failed','nightly-failed'],['superseded','old-superseded'],['infra-retried','retry-failed'],['running','scheduled-running']]) {
      const start = requests.length;
      await page.locator(`[data-status-filter="${filter}"]`).click();
      await page.locator(`.runs-table > [data-run-id="${id}"]`).waitFor();
      assert.ok(requests.slice(start).some(path=>path.startsWith('/api/runs?')&&!path.includes('view=active')&&!path.includes('latest_per_ref')));
      assert.ok((await page.locator('.runs-footer').textContent()).includes('All retained runs'));
    }
    await page.locator('[data-status-filter="all"]').click();
    await page.locator('.runs-table > [data-run-id="head-main"]').waitFor();
    await page.locator('[data-kind-filter="release"]').click();
    await page.waitForFunction(() => document.querySelectorAll('.runs-table > [data-run-id]').length === 1);
    assert.equal(await page.locator('.runs-table > [data-run-id]').getAttribute('data-run-id'), 'head-tag');
    await page.locator('[data-kind-filter="release"]').click();
    await page.locator('[data-repo-filter="github/org/work-laptop-identities"]').click();
    await page.getByText('No runs match the current filters.').waitFor();
    await page.locator('[data-repo-filter="all"]').click();
    await page.locator('.runs-table').waitFor();
    // A direct URL must work without navigating from a cached list first.
    await page.goto(`${origin}/runs/head-main`);
    await page.locator('[data-detail-run]').waitFor();
    assert.deepEqual(await page.locator('#stepList .nm').allTextContents(), ['Run summary','ci/test','ci/publish']);
    assert.equal(await page.locator('#stepList .tag').textContent(), 'secret');
    assert.ok(!(await page.locator('.rh').textContent()).toLowerCase().includes('credentialed'));
    assert.equal(await page.locator('.rh [title="fixture priority"]').textContent(),'highest priority');
    await capture('after-detail');
    await page.locator('[data-action="toggle-attempts"]').click();
    await page.locator('#runHistory [data-run-id="old-superseded"]').first().waitFor();
    assert.equal(await page.locator('#runHistory .runs-table > [data-run-id]').count(), 2);
    assert.equal(await page.locator('#runHistory .repo-name .v4b').count(),0,'history rows have no inline badges');
    assert.equal(await page.locator('#runHistory .runs-table > [data-run-id="old-failed"] > div:last-child').textContent(),'priority high · history priority fixture failure','history preserves priority and the existing failure message instead of the schedule fallback');
    delayOlder=true;
    await page.locator('[data-action="older-attempts"]').focus();
    await page.locator('[data-action="older-attempts"]').press('Enter');
    await page.locator('[data-action="older-attempts"][aria-disabled="true"]').waitFor();
    assert.equal(await page.evaluate(()=>document.activeElement?.dataset.action),'older-attempts','keyboard focus stays on the loading Older button');
    delayOlder=false; releaseOlder();
    await page.locator('#runHistory [data-run-id="oldest"]').waitFor();
    assert.equal(await page.evaluate(()=>document.activeElement?.dataset.action),'toggle-attempts','exhausted history returns focus to its toggle');
    assert.equal(await page.locator('#runHistory .runs-table > [data-run-id]').count(), 3);
    assert.ok(requests.some(path => path.includes('before=opaque-history-cursor')));
    await page.evaluate(() => {
      window.historyToggle = document.querySelector('#runHistory [data-action="toggle-attempts"]');
      window.historyTable = document.querySelector('#runHistory .runs-table');
      historyToggle.focus(); historyTable.scrollLeft = 140; window.historyLeft = historyTable.scrollLeft;
      return route(true);
    });
    await page.waitForFunction(()=>!state.attempts.get('head-main').refreshing);
    assert.equal(await page.evaluate(()=>document.activeElement===historyToggle && historyToggle.isConnected && historyTable===document.querySelector('#runHistory .runs-table') && historyTable.scrollLeft===historyLeft), true, 'unchanged poll preserves history nodes, focus and scroll');
    older.Status = 'passed';
    await page.evaluate(()=>route(true));
    await page.waitForFunction(()=>document.querySelector('#runHistory .trow[data-run-id="old-failed"] .pill').textContent.trim()==='passed');
    assert.equal(await page.evaluate(()=>document.activeElement===document.querySelector('#runHistory [data-action="toggle-attempts"]') && document.querySelector('#runHistory .runs-table').scrollLeft===historyLeft), true, 'changed poll refreshes status and restores focus/scroll');
    older.Status = 'failed';
    await page.locator('[data-action="refresh"]').click();
    await page.waitForFunction(()=>document.querySelector('#runHistory .trow[data-run-id="old-failed"] .pill').textContent.trim()==='failed');
    await page.locator('#runHistory [data-run-id="oldest"]').waitFor();
    assert.equal(await page.locator('#stepList .tag').textContent(), 'secret');
    const detailLayout = await page.evaluate(() => ({width:document.querySelector('.screen').getBoundingClientRect().width, viewport:innerWidth, rows:document.querySelector('#runHistory .runs-table').getBoundingClientRect().width}));
    assert.ok(detailLayout.width <= width && detailLayout.rows <= width, `history fits ${width}px`);
    await capture('after-history');
    await page.locator('#runHistory .runs-table > [data-run-id="old-failed"]').click();
    await page.waitForURL('**/runs/old-failed');
    await page.locator('[data-detail-run="old-failed"]').waitFor();
    await page.locator('[data-action="toggle-attempts"]').click();
    await page.locator('#runHistory [data-run-id="head-main"]').first().waitFor();
    // A failed first history request leaves the detail/log frame available.
    await page.locator('[data-action="toggle-attempts"]').click();
    historyError = true;
    await page.locator('[data-action="toggle-attempts"]').click();
    await page.getByRole('status').filter({hasText:'Could not load run history'}).waitFor();
    assert.equal(await page.locator('#termBody').count(), 1);
    historyError = false;
    await page.getByRole('button', {name:'Retry',exact:true}).click();
    await page.locator('#runHistory [data-run-id="head-main"]').first().waitFor();
    await page.goto(`${origin}/runs/head-tag`);
    await page.getByRole('button',{name:'Run history for this tag'}).waitFor();
    assert.ok((await page.locator('.rh').textContent()).includes('RELEASE'));
    assert.ok(!(await page.locator('.rh').textContent()).toLowerCase().includes('credentialed'));
    assert.deepEqual(errors, []);
    results.push({width,layout,detailLayout,errors});
    await context.close();
  }
  // Paging must keep the server's opaque cursor and grouping for both lists.
  paging = true;
  const page = await browser.newPage();
  await page.addInitScript(() => localStorage.setItem('oberth-token','fixture-token'));
  await page.goto(`${origin}/runs`);
  await page.locator('[data-action="runs-older"]').click();
  await page.locator('[data-run-id="page-two"]').waitFor();
  assert.equal(await page.locator('.runs-table > [data-run-id="scheduled-running"]').count(),1,'page two keeps an active scheduled run hidden by its newer push');
  assert.equal(await page.locator('.runs-table > [data-run-id="head-queued"]').count(),1,'page two keeps active heads from page one');
  assert.ok(requests.some(path => path.includes('before=opaque-list-cursor') && path.includes('latest_per_ref=1')));
  await page.locator('[data-action="runs-newer"]').click();
  await page.locator('[data-run-id="page-one-0"]').waitFor();
  assert.equal(await page.locator('.runs-table > [data-run-id]').count(), 100 + active.length);
  await page.locator('[data-action="runs-older"]').click();
  await page.locator('[data-run-id="page-two"]').waitFor();
  const filterStart = requests.length;
  await page.locator('[data-status-filter="running"]').click();
  await page.locator('[data-status-filter="running"].on').waitFor();
  await page.locator('.runs-table > [data-run-id="scheduled-running"]').waitFor();
  assert.ok(requests.slice(filterStart).some(path=>path.startsWith('/api/runs?')&&!path.includes('view=active')&&!path.includes('latest_per_ref')&&!path.includes('before=')),'changing status on page two resets to the flat first page');
  paging = false; sharedNames = true; delayStatus = true;
  const direct = await browser.newPage();
  await direct.addInitScript(()=>localStorage.setItem('oberth-token','fixture-token'));
  await direct.goto(`${origin}/runs/head-main`);
  await direct.locator('[data-detail-run="head-main"]').waitFor();
  const historyStart = requests.length;
  await direct.locator('[data-action="toggle-attempts"]').click();
  await direct.getByText('Loading run history…').waitFor();
  assert.ok(!requests.slice(historyStart).some(path=>path.includes('ref=main')),'history waits for pending shared-name repository context');
  delayStatus = false; releaseStatus();
  await direct.locator('#runHistory .trow[data-run-id="old-failed"]').waitFor();
  assert.ok(requests.slice(historyStart).some(path=>path.includes('ref=main')&&path.includes('repo=github%2Forg%2Foberth')),'history uses the qualified selector after status resolves');
  await direct.goto(`${origin}/runs/head-mai`);
  await direct.locator('[data-detail-run="head-main"]').waitFor();
  await direct.locator('[data-action="toggle-attempts"]').click();
  await direct.locator('#runHistory .trow[data-run-id="old-failed"]').waitFor();
  older.Status='passed';
  await direct.locator('[data-action="refresh"]').click();
  await direct.waitForFunction(()=>document.querySelector('#runHistory .trow[data-run-id="old-failed"] .pill').textContent.trim()==='passed');
  older.Status='failed';
  await direct.close();
  sharedNames=false; delayStatus=true;
  const cold = await browser.newPage();
  await cold.addInitScript(()=>{
    localStorage.setItem('oberth-token','fixture-token');
    localStorage.setItem('oberth-repo','github/org/oberth');
  });
  await cold.goto(`${origin}/runs`);
  await cold.locator('.runs-table > [data-run-id="scheduled-running"]').waitFor();
  assert.ok(await cold.evaluate(()=>state.status===null && state.activeRunsComplete),'valid saved scope remains visible while status is pending');
  assert.ok((await cold.locator('.runs-footer').textContent()).includes('all active runs included'));
  delayStatus=false; releaseStatus();
  await cold.close();
  if (evidence) writeFileSync(`${evidence}/browser-results.json`, JSON.stringify({browser:await browser.version(),results,requests},null,2));
  console.log('Rendered dashboard checks passed: 1440px, 1100px, 390px, 320px; active scheduled/queued rows on both pages, flat status filters, direct shared-name links, history refresh/focus/scroll, step order and secret/release markers.');
} finally {
  await browser?.close();
  server.closeAllConnections();
  await new Promise(resolve => server.close(resolve));
}
