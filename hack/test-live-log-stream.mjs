// Optional browser regression: PLAYWRIGHT_MODULE=/path/to/playwright/index.mjs
// node hack/test-live-log-stream.mjs (uses the installed Chromium headless shell).
import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import {createServer} from 'node:http';
const {chromium} = await import(process.env.PLAYWRIGHT_MODULE || 'playwright');
const source = readFileSync(new URL('../internal/api/static/app.js', import.meta.url), 'utf8');
const css = readFileSync(new URL('../internal/api/static/app.css', import.meta.url), 'utf8');
const connections = [];
const pollOffsets = [];
let fallback = {chunk: '', offset: 0};
const server = createServer((req, res) => {
  const url = new URL(req.url, 'http://localhost');
  if (url.pathname.endsWith('/livelog/stream')) {
    assert.equal(req.headers.authorization, 'Bearer browser-token');
    assert.equal(url.searchParams.has('token'), false);
    res.writeHead(200, {'Content-Type': 'text/event-stream'});
    res.write(': ping\n\n');
    const connection = {res, closed: false};
    connections.push(connection);
    res.on('close', () => {connection.closed = true;});
  } else if (url.pathname.endsWith('/livelog')) {
    pollOffsets.push(Number(url.searchParams.get('offset')));
    res.writeHead(200, {'Content-Type': 'application/json'});
    res.end(JSON.stringify(fallback));
  } else if (/^\/assets\/fonts\/IBMPlex(?:Mono|Sans)-(?:Regular|SemiBold|Bold)\.woff2$/.test(url.pathname)) {
    res.writeHead(200, {'Content-Type': 'font/woff2'});
    res.end(readFileSync(new URL('../internal/api/static/fonts/' + url.pathname.split('/').pop(), import.meta.url)));
  } else if (url.pathname === '/app.js') {
    res.writeHead(200, {'Content-Type': 'text/javascript'});
    res.end(source.slice(0, source.indexOf('/* ---------- events ---------- */')));
  } else {
    res.writeHead(200, {'Content-Type': 'text/html'});
    res.end(`<style>${css} .term-body{height:240px;max-height:240px;} .log-line{min-height:20px}</style><div id="app"></div><script src="/app.js"></script>`);
  }
});
await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
const browser = await chromium.launch({headless: true, args: ['--no-sandbox']});
const page = await browser.newPage();
page.setDefaultTimeout(5000);
const errors = [];
page.on('pageerror', error => errors.push(error.message));
const frame = (chunk, offset, done = false) => `event: chunk\ndata: ${JSON.stringify({chunk, offset, done})}\n\n`;
try {
  await page.addInitScript(() => localStorage.setItem('oberth-token', 'browser-token'));
  await page.goto(`http://127.0.0.1:${server.address().port}/runs/live-run`);
  await page.evaluate(async () => {
    state.repos = [{ID: 1, Name: 'oberth'}]; state.step = '__summary__'; state.stepRunID = 'live-run';
    window.detail = {Run: {ID: 'live-run', RepoID: 1, Ref: 'test', Status: 'running', StartedAt: new Date().toISOString()}, Steps: []};
    setAuto = ms => {window.metadataPollMs = ms;};
    await renderRunDetailView(detail, state.routeSeq);
    window.originalTerm = document.getElementById('termBody');
  });
  await page.waitForFunction(() => state.live?.connected);
  await page.evaluate(() => document.fonts.ready);
  assert.equal(connections.length, 1);
  assert.equal(await page.evaluate(() => metadataPollMs), 4000);
  // Split a frame inside a UTF-8 code point; partial command records only render
  // after a subsequent newline, and delivery metadata stays outside the log.
  const wire = Buffer.from(frame('[build/one] first 🚀\n[build/one] par', 50));
  const split = wire.indexOf(Buffer.from('🚀')) + 1;
  connections[0].res.write(wire.subarray(0, split));
  connections[0].res.write(wire.subarray(split));
  await page.waitForFunction(() => document.getElementById('termBody').textContent.includes('first 🚀'));
  await page.evaluate(() => {window.firstRow = document.querySelector('[data-log-record]');});
  connections[0].res.write(frame('tial\n::oberth-log:: {"burn":"build","step":"one","state":"following"}\n', 150));
  await page.waitForFunction(() => document.getElementById('termBody').textContent.includes('partial'));
  assert.equal(await page.locator('#termBody .log-hdr').count(), 1);
  assert.equal(await page.locator('#termBody [data-log-record]').count(), 2);
  assert.equal(await page.locator('#termBody').textContent().then(text => text.includes('::oberth-log::')), false);
  await page.evaluate(async () => {await renderRunDetailView(detail, state.routeSeq);});
  assert.equal(await page.evaluate(() => originalTerm === document.getElementById('termBody') && firstRow.isConnected), true);
  assert.equal(connections.length, 1);
  // Synchronous bursts share a single animation frame and append operation.
  await page.evaluate(() => {
    window.appendCalls = 0;
    const insert = originalTerm.insertAdjacentHTML.bind(originalTerm);
    originalTerm.insertAdjacentHTML = (...args) => {appendCalls++; insert(...args);};
    receiveLiveChunk(state.live, {chunk: '[build/one] burst-a\n', offset: 170});
    receiveLiveChunk(state.live, {chunk: '[build/two] burst-b\n', offset: 190});
  });
  await page.waitForFunction(() => document.getElementById('termBody').textContent.includes('burst-b'));
  assert.equal(await page.evaluate(() => appendCalls), 1);
  assert.equal(await page.locator('#termBody .log-hdr').count(), 2);
  assert.deepEqual(await page.locator('#termBody [data-log-record] .log-no').allTextContents(), ['1', '2', '3', '4']);
  // Evict from the top without rebuilding surviving DOM nodes; keep the step
  // header when the boundary moves through a group, and retain scroll position.
  await page.evaluate(() => receiveLiveChunk(state.live, {chunk: '[build/two] row\n'.repeat(4996), offset: 200}));
  await page.waitForFunction(() => document.querySelectorAll('[data-log-record]').length === 5000);
  await page.evaluate(() => {originalTerm.scrollTop = 1000; window.survivor = originalTerm.querySelectorAll('[data-log-record]')[100]; window.beforeScroll = originalTerm.scrollTop;});
  await page.evaluate(() => receiveLiveChunk(state.live, {chunk: '[build/two] newest\n', offset: 220}));
  await page.waitForFunction(() => document.getElementById('termBody').textContent.includes('newest'));
  assert.equal(await page.evaluate(() => survivor.isConnected && originalTerm === document.getElementById('termBody')), true);
  assert.equal(await page.locator('#termBody [data-log-record]').count(), 5000);
  assert.equal(await page.locator('#termBody [data-log-record] .log-no').first().textContent(), '2');
  assert.equal(await page.locator('#termBody [data-log-record] .log-no').last().textContent(), '5001');
  assert.equal(await page.evaluate(() => originalTerm.scrollTop < beforeScroll && originalTerm.scrollTop > 0), true, JSON.stringify(await page.evaluate(() => ({top:originalTerm.scrollTop,before:beforeScroll,height:originalTerm.scrollHeight,client:originalTerm.clientHeight}))));
  assert.equal(await page.locator('#termBody .log-hdr').first().textContent().then(s => s.includes('build / one')), true);
  // Scope changes reset the view, but reuse the same stream for this run.
  await page.evaluate(async () => {
    detail.Steps = [{Burn:'build', Step:'two', Status:'running'}]; state.step = 'build / two';
    await renderRunDetailView(detail, state.routeSeq);
  });
  assert.equal(connections.length, 1);
  assert.equal(await page.locator('#termBody .log-hdr').count(), 0);
  assert.equal(await page.locator('#termBody').textContent().then(s => s.includes('first 🚀')), false);
  // A dropped SSE connection resumes polling at the last delivered cursor.
  fallback = {chunk:'[build/two] fallback-tail', offset:250, done:true};
  connections[0].res.end();
  await page.waitForFunction(() => state.live.done && document.getElementById('termBody').textContent.includes('fallback-tail'));
  assert.deepEqual(pollOffsets, [220]);
  assert.equal(await page.evaluate(() => state.live.source === null && state.live.pollTimer === null), true);
  // A done event on SSE itself flushes a final unterminated line and closes.
  await page.evaluate(async () => {
    closeLiveLog(); state.step = '__summary__';
    await renderRunDetailView(detail, state.routeSeq);
  });
  await page.waitForFunction(() => state.live.connected);
  connections[1].res.write(frame('[build/two] sse-final', 280, true));
  await page.waitForFunction(() => state.live.done && document.getElementById('termBody').textContent.includes('sse-final'));
  assert.equal(await page.evaluate(() => state.live.source === null && state.live.pending === ''), true);
  // Navigation and sign-out close active streams and stale events cannot write.
  await page.evaluate(async () => {
    closeLiveLog(); state.step = '__summary__';
    await renderRunDetailView(detail, state.routeSeq);
  });
  await page.waitForFunction(() => state.live.connected);
  assert.equal(connections.length, 3);
  await page.evaluate(async () => {
    window.abandoned = state.live; history.pushState(null, '', '/repos'); renderRepos = async () => {}; await route();
    receiveLiveChunk(abandoned, {chunk:'stale\n', offset:999});
  });
  assert.equal(await page.evaluate(() => state.live === null && abandoned.closed && abandoned.offset === -1), true);
  await page.evaluate(async () => {history.pushState(null, '', '/runs/live-run'); await renderRunDetailView(detail, state.routeSeq);});
  await page.waitForFunction(() => state.live.connected);
  await page.evaluate(() => clearToken());
  assert.equal(await page.evaluate(() => state.live === null && !hasToken()), true);
  assert.deepEqual(errors, []);
  console.log('live log browser streaming checks passed');
} finally {
  await browser.close();
  for (const connection of connections) connection.res.destroy();
  server.closeAllConnections();
  await new Promise(resolve => server.close(resolve));
}
