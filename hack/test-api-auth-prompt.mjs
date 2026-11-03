import { readFileSync } from 'node:fs';
import vm from 'node:vm';
import assert from 'node:assert/strict';
const source = readFileSync(new URL('../internal/api/static/app.js', import.meta.url), 'utf8');
for (const token of [false, true]) {
 for (const status of [401, 429]) {
  let prompt;
  const sandbox = vm.createContext({
   state: { authGen: 1, routeSeq: 1 }, AbortController,
   authHeaders: () => ({}), hasToken: () => token, setConn: () => {},
   showTokenPrompt: message => { prompt = message; },
   fetch: async () => ({ status, ok: false, headers: new Headers({'Retry-After': '60'}), json: async () => ({error:'too many authentication failures'}) }),
  });
  vm.runInContext(source.slice(source.indexOf('async function api('), source.indexOf('/* ---------- navigation ---------- */')), sandbox);
  await assert.rejects(vm.runInContext('api("/api/status")', sandbox), /authentication required/);
  assert.notEqual(prompt, undefined, `${status} must open the token prompt`);
  if (status === 429) assert.match(prompt, /retry|wait/i);
 }
}
// An unrelated backend error must remain an ordinary API error.
let prompted = false;
const sandbox = vm.createContext({state:{authGen:1,routeSeq:1},AbortController,authHeaders:()=>({}),setConn:()=>{},showTokenPrompt:()=>{prompted=true;},fetch:async()=>({status:503,ok:false,json:async()=>({error:'unavailable'})})});
vm.runInContext(source.slice(source.indexOf('async function api('), source.indexOf('/* ---------- navigation ---------- */')), sandbox);
await assert.rejects(vm.runInContext('api("/api/status")', sandbox), /unavailable/);
assert.equal(prompted, false);
