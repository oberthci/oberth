// Resource advice belongs to detail; server display_reason is the row message.
import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import vm from 'node:vm';
const source = readFileSync(new URL('../internal/api/static/app.js', import.meta.url), 'utf8');
const context = vm.createContext({URLSearchParams, Map, Set, Date, location:{pathname:'/runs'}, document:{body:{dataset:{version:'test'}},getElementById(){return null}}, localStorage:{getItem(){return null},setItem(){}}, setInterval(){},clearInterval(){}});
vm.runInContext(source.slice(0, source.indexOf('/* ---------- events ---------- */')), context);
const evaluate = code => vm.runInContext(code, context);
const notice = '2 steps have undeclared CPU/memory requests; inspect oberth run resources --run ' + 'a'.repeat(32) + ' --include-failed.';
const base = {ID:'a'.repeat(32),RepoID:1,Ref:'step-abc12345',Status:'running',StartedAt:'2026-11-01T10:00:00Z',priority_class:'normal',priority_reason:'ordinary run',display_reason:'',resource_message:notice};
for (const extra of [{},{Trigger:'schedule',ScheduleEntry:'nightly'},{Release:true},{Error:'full error'}]) {
 context.row={...base,...extra};
 assert.equal(evaluate('rowMessage(row)'),'');
 assert.ok(!evaluate('runRow(row)').includes(notice));
}
context.row={...base,Status:'interrupted',display_reason:'server restart, retry needed'};
assert.equal(evaluate('runMessageCell(row)'),'<span class="retry-warning">server restart, retry needed</span>');
context.row={...base,Status:'queued',StartedAt:null,display_reason:'waiting for 1 CPU, 512 MiB'};
assert.equal(evaluate('rowMessage(row)'),'waiting for 1 CPU, 512 MiB');
context.row={...base,display_reason:'<unsafe>'};
assert.equal(evaluate('runMessageCell(row)'),'&lt;unsafe&gt;');
context.row=base;
assert.ok(evaluate('summaryLines({Run:row,Steps:[]})').includes(notice));
console.log('Resource advice remains detail-only.');

context.row={...base,Status:'queued',StartedAt:null,display_reason:'next in line',Reason:'full operator provenance'};
assert.ok(evaluate(`summaryLines({Run:row,Steps:[],Scheduling:{message:'full scheduling observation'}})`).includes('[scheduling] full scheduling observation'));
assert.ok(evaluate(`summaryLines({Run:row,Steps:[]})`).includes('reason: full operator provenance'));
context.row={...base,Status:'failed',display_reason:'unit-tests failed',Error:'full unabridged controller evidence'};
assert.ok(evaluate(`summaryLines({Run:row,Steps:[]})`).includes('error: full unabridged controller evidence'));
