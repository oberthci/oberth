// Resource notices must preserve the shipped row renderer's exact status vocabulary.
import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import vm from 'node:vm';
const source = readFileSync(new URL('../internal/api/static/app.js', import.meta.url), 'utf8');
const context = vm.createContext({URLSearchParams, Map, Set, Date, location:{pathname:'/runs'}, document:{body:{dataset:{version:'test'}},getElementById(){return null}}, localStorage:{getItem(){return null},setItem(){}}, setInterval(){},clearInterval(){}});
vm.runInContext(source.slice(0, source.indexOf('/* ---------- events ---------- */')), context);
const evaluate = code => vm.runInContext(code, context);
const notice = '2 steps have undeclared CPU/memory requests; inspect oberth run resources --run ' + 'a'.repeat(32) + ' --include-failed.';
const base = {ID:'a'.repeat(32),RepoID:1,Ref:'step-abc12345',Status:'running',StartedAt:'2026-11-01T10:00:00Z',priority_class:'normal',priority_reason:'ordinary run',display_reason:'Priority normal: ordinary run.',resource_message:notice};
for (const [extra, prefix] of [[{},''],[{Trigger:'schedule',ScheduleEntry:'step-abc12345'},'schedule: step-abc12345 '],[{Release:true},'release '],[{Error:'step-abc12345'},'step-abc12345 ']]) {
  context.row = {...base,...extra};
  assert.equal(evaluate('rowMessage(row)'),prefix+notice);
  assert.equal(evaluate('rowMessage(row, true)'),prefix+notice);
  assert.equal(evaluate('row.display_reason'),base.display_reason);
  assert.ok(!evaluate('runRow(row)').includes('Priority normal:'),'ordinary priority must stay out of the message cell');
}
context.row = {...base,Status:'interrupted',display_reason:'server restart, retry needed'};
assert.equal(evaluate('runMessageCell(row)'),`<span class="retry-warning">server restart, retry needed ${notice}</span>`);
context.row = {...base,Status:'queued',StartedAt:null,Scheduling:{state:'waiting',admission:{state:'waiting',size:'M'}}};
assert.equal(evaluate('rowMessage(row)'),`waiting for capacity (M) ${notice}`);
context.row = {...base,resource_message:'<step-abc12345>'};
assert.equal(evaluate('runMessageCell(row)'),'&lt;step-abc12345&gt;');
context.row = base;
assert.ok(evaluate('summaryLines({Run:row,Steps:[]})').includes(notice),'run detail must show the separate notice');
console.log('Resource message rows passed.');

context.row = {...base,Status:'queued',StartedAt:null,Scheduling:{state:'waiting',admission:{size:'XL',declared_resources:{cpu_milli:1100,memory_bytes:1342177280,pods:1},head_of_queue:true,blocked_by:{backfilling:true}}}};
assert.equal(evaluate('queueSize(row)'),'', 'declared admission must not show the ignored size annotation');
assert.ok(evaluate('rowMessage(row)').includes('1100m CPU'));
assert.ok(evaluate('rowMessage(row)').includes('smaller runs admitted meanwhile'));
assert.ok(!evaluate('rowMessage(row)').includes('whole host'));
