// Exercise plain native streams, request cancellation and retained bytes.
import {readFileSync} from 'node:fs';
import vm from 'node:vm';
import assert from 'node:assert/strict';
const source=readFileSync(new URL('../internal/api/static/app.js',import.meta.url),'utf8');
const streams=[];
const sandbox=vm.createContext({console,URLSearchParams,Map,Set,Date,AbortController,TextDecoder,ReadableStream,
 location:{pathname:'/runs/run'},localStorage:{getItem(){return 'valid-token';}},
 document:{getElementById(){return null;},querySelector(){return null;}},
 setTimeout(){},clearTimeout(){},setInterval(){},clearInterval(){},
 fetch:async(url,options)=>{
  assert.equal(options.headers.Authorization,'Bearer valid-token');assert.ok(!url.includes('token='));
  const item={url,options,cancelled:false};streams.push(item);
  const body=new ReadableStream({start(controller){item.controller=controller;options.signal.addEventListener('abort',()=>{item.cancelled=true;controller.error(new Error('aborted'));},{once:true});},cancel(){item.cancelled=true;}});
  return {ok:true,status:200,body};
 }
});
vm.runInContext(source.slice(0,source.indexOf('/* ---------- events ---------- */')),sandbox);
const run=code=>vm.runInContext(code,sandbox);
const settle=()=>new Promise(resolve=>setImmediate(resolve));
run(`const entries=[{node_id:'node',container:'main',stored:false},{node_id:'node',container:'wait',stored:false}];let requests=[];api=async path=>{requests.push(path);return path.endsWith('/containers')?{containers:entries}:{output:'@@oberth:step plain\\n[build/test] retained\\n'};};`);
let result=await run(`containerLogContent({id:'run',status:'running'},'node')`);await settle();
assert.match(result.options,/wait/);assert.equal(streams.length,1);assert.match(streams[0].url,/node_id=node&container=main/);
const bytes=new TextEncoder().encode('@@oberth:step plain\n::oberth-log:: plain\n[build/test] 🚀');const split=bytes.indexOf(0xf0)+1;
streams[0].controller.enqueue(bytes.slice(0,split));await settle();streams[0].controller.enqueue(bytes.slice(split));await settle();
assert.equal(run('state.live.text'),'@@oberth:step plain\n::oberth-log:: plain\n[build/test] 🚀');
await run(`containerLogContent({id:'run',status:'running'},'node')`);assert.equal(streams.length,1,'metadata refresh reuses the request');
streams[0].controller.close();await settle();await run(`containerLogContent({id:'run',status:'running'},'node')`);
assert.equal(streams.length,1,'EOF never reconnects or polls log bytes');assert.equal(run('state.live.done'),true);
run(`state.containerKey=containerKey(entries[1])`);await run(`containerLogContent({id:'run',status:'running'},'node')`);await settle();assert.equal(streams.length,2);assert.equal(streams[0].options.signal.aborted,true);
run('closeLiveLog()');await settle();assert.equal(streams[1].options.signal.aborted,true);assert.equal(streams[1].cancelled,true);
run('entries[0].stored=true;state.containerKey=containerKey(entries[0])');result=await run(`containerLogContent({id:'run',status:'passed'},'node')`);
assert.ok(result.html.includes('@@oberth:step plain'));assert.ok(result.html.includes('[build/test] retained'));assert.equal(result.live,false);
console.log('container bytes stay plain; one request, no reconnect, cancellation joins');
