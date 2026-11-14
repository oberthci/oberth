import { readFileSync } from 'node:fs';
import vm from 'node:vm';
import assert from 'node:assert/strict';
const source = readFileSync(new URL('../internal/api/static/app.js', import.meta.url), 'utf8');
const sandbox = vm.createContext({console,URLSearchParams,Map,Set,Date,location:{pathname:'/status'},document:{body:{dataset:{version:'test'}},getElementById(){return null}},localStorage:{getItem(){return null}},setInterval(){},clearInterval(){},requestAnimationFrame(){return 1},cancelAnimationFrame(){}});
vm.runInContext(source.slice(0,source.indexOf('/* ---------- events ---------- */')),sandbox);
vm.runInContext(`
 let combinedHTML='';
 replaceApp=html=>{combinedHTML=html};
 api=async path=>path==='/api/status'?{version:'test',database:'ready',audit:'ready',cluster:'ready',vcs:'ready'}:[];
`,sandbox);
await vm.runInContext('renderStatus(0)',sandbox);
const first=vm.runInContext('combinedHTML',sandbox);
for(const text of ['server','database','audit'])assert.ok(first.includes(text),text);
for(const text of ['execution target recovery','controller verification','8 admitted','host pressure','admission paused'])assert.ok(!first.includes(text),text);
console.log('ordinary runtime status rendering passed');
