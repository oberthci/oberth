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
vm.runInContext("api=async path=>path==='/api/status'?{version:'test',execution_target_recovery:{quarantined_bindings:2,reason:'Custody <inconsistent>',remedy:'Inspect <original objects>'}}:[];",sandbox);
await vm.runInContext('renderStatus(0)',sandbox);
const recovery=vm.runInContext('combinedHTML',sandbox);
for(const text of ['execution target recovery','2 quarantined bindings','Custody &lt;inconsistent&gt;','Inspect &lt;original objects&gt;'])assert.ok(recovery.includes(text),text);
assert.ok(!recovery.includes('Custody <inconsistent>'));
console.log('quarantined target recovery status rendering passed');
