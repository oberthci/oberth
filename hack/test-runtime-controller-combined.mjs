import { readFileSync } from 'node:fs';
import vm from 'node:vm';
import assert from 'node:assert/strict';
const source = readFileSync(new URL('../internal/api/static/app.js', import.meta.url), 'utf8');
const sandbox = vm.createContext({console,URLSearchParams,Map,Set,Date,location:{pathname:'/status'},document:{body:{dataset:{version:'test'}},getElementById(){return null}},localStorage:{getItem(){return null}},setInterval(){},clearInterval(){},requestAnimationFrame(){return 1},cancelAnimationFrame(){}});
vm.runInContext(source.slice(0,source.indexOf('/* ---------- events ---------- */')),sandbox);
vm.runInContext(`
 let combinedHTML='';
 let combinedProfile={state:'blocked',dispatch_blocked:true,reason:'ReplicaSet <not-ready>',remedy:'Restore one Ready replacement'};
 replaceApp=html=>{combinedHTML=html};
 api=async path=>path==='/api/status'?{version:'test',database:'ready',audit:'ready',cluster:'ready',vcs:'ready',controller_profile:combinedProfile}:[];
`,sandbox);
await vm.runInContext('renderStatus(0)',sandbox);
const first=vm.runInContext('combinedHTML',sandbox);
for(const text of ['controller verification','controller verification required','ReplicaSet &lt;not-ready&gt;','Restore one Ready replacement'])assert.ok(first.includes(text),text);
assert.ok(!first.includes('ReplicaSet <not-ready>'));
vm.runInContext("combinedProfile={state:'verified',dispatch_blocked:false};",sandbox);
await vm.runInContext('renderStatus(0)',sandbox);
const recovered=vm.runInContext('combinedHTML',sandbox);
assert.ok(recovered.includes('controller verified')&&!recovered.includes('controller verification required'));
for(const text of ['8 admitted','host pressure','admission paused']) assert.ok(!recovered.includes(text));
console.log('controller verification status rendering passed');

vm.runInContext("api=async path=>path==='/api/status'?{version:'test',execution_target_recovery:{quarantined_bindings:2,reason:'Custody <inconsistent>',remedy:'Inspect <original objects>'}}:[];",sandbox);
await vm.runInContext('renderStatus(0)',sandbox);
const recovery=vm.runInContext('combinedHTML',sandbox);
for(const text of ['execution target recovery','2 quarantined bindings','Custody &lt;inconsistent&gt;','Inspect &lt;original objects&gt;'])assert.ok(recovery.includes(text),text);
assert.ok(!recovery.includes('Custody <inconsistent>'));
console.log('quarantined target recovery status rendering passed');
