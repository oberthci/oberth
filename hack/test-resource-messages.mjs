// Resource advice belongs to detail; server display_reason is the row message.
import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import vm from 'node:vm';
const source = readFileSync(new URL('../internal/api/static/app.js', import.meta.url), 'utf8');
const context = vm.createContext({URLSearchParams, Map, Set, Date, location:{pathname:'/runs'}, document:{body:{dataset:{version:'test'}},getElementById(){return null}}, localStorage:{getItem(){return null},setItem(){}}, setInterval(){},clearInterval(){}});
vm.runInContext(source.slice(0, source.indexOf('/* ---------- events ---------- */')), context);
const evaluate = code => vm.runInContext(code, context);
const notice = 'unit exceeded its authored memory limit.';
const base = {id:'a'.repeat(32),repo_id:1,ref:'step-abc12345',status:'running',display_status:'running',started_at:'2026-11-01T10:00:00Z',display_reason:'',resource_message:notice};
for (const extra of [{},{trigger:'schedule',schedule_entry:'nightly'},{release:true},{error:'full error'}]) {
 context.row={...base,...extra};
 assert.equal(evaluate('rowMessage(row)'),'');
 assert.ok(!evaluate('runRow(row)').includes(notice));
}
context.row={...base,status:'interrupted',display_status:'cancelled',display_reason:'server restart, retry needed'};
assert.equal(evaluate('runMessageCell(row)'),'<span class="retry-warning">server restart, retry needed</span>');
context.row={...base,status:'queued',display_status:'queued',started_at:null,display_reason:'waiting for 1 CPU, 512 MiB'};
assert.equal(evaluate('rowMessage(row)'),'waiting for 1 CPU, 512 MiB');
context.row={...base,display_reason:'<unsafe>'};
assert.equal(evaluate('runMessageCell(row)'),'&lt;unsafe&gt;');
context.row=base;
assert.ok(evaluate('summaryLines({run:row,steps:[]})').includes(notice));
console.log('Resource advice remains detail-only.');

context.row={...base,status:'queued',display_status:'queued',started_at:null,display_reason:'waiting for Argo',reason:'full operator provenance'};
assert.ok(evaluate(`summaryLines({run:row,steps:[],scheduling:{message:'full scheduling observation'}})`).includes('[scheduling] full scheduling observation'));
assert.ok(evaluate(`summaryLines({run:row,steps:[]})`).includes('reason: full operator provenance'));
context.row={...base,status:'failed',display_status:'failed',display_reason:'unit-tests failed',error:'full unabridged controller evidence'};
assert.ok(evaluate(`summaryLines({run:row,steps:[]})`).includes('error: full unabridged controller evidence'));
