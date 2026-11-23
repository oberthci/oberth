import {check} from './cdp.mjs';

export async function headerActions(client) {
 const wire=await client.evaluate(`Promise.all(headerRuns.slice(0,5).map(run=>call('run_get',{id:run.id})))`);
 check(wire.every(run=>run.display_reason===''),'MCP added a redundant run reason');
 const inexact=await client.evaluate(`(()=>{const run={...headerRun,steps_planned_exact:false},box=h("div");box.append(...runHeader(run,runActions(screen,run,[],[]),false));return box.querySelector(".run-counts").textContent;})()`);
 check(inexact.includes("/>="),"inexact total lost its bound");
 await client.loaded('/runs/33333333','.run-summary button');
 const retained=await client.evaluate(`(async()=>{document.querySelector(".run-meta summary").click();await screen.read();return document.querySelector(".run-meta details").open;})()`);
 check(retained,"refresh collapsed Detail");
 await client.evaluate(`document.querySelector(".run-meta summary").click()`);

 const {x,y}=await client.evaluate(`(()=>{window.confirm=()=>true;const box=document.querySelector('.run-summary button').getBoundingClientRect();return {x:box.x+box.width/2,y:box.y+box.height/2};})()`);
 const before=client.requests.length;
 await client.cdp('Input.dispatchMouseEvent',{type:'mousePressed',x,y,button:'left',clickCount:1});
 await client.cdp('Input.dispatchMouseEvent',{type:'mouseReleased',x,y,button:'left',clickCount:1});
 await client.evaluate(`new Promise(resolve=>{const observer=new MutationObserver(()=>{if(notice.textContent.includes('This run cannot be cancelled.')){observer.disconnect();resolve();}});observer.observe(notice,{childList:true,subtree:true});if(notice.textContent.includes('This run cannot be cancelled.')){observer.disconnect();resolve();}})`);
 check(client.requests.slice(before).some(request=>request.path==='/mcp'&&JSON.parse(request.body).params.name==='run_cancel'),'Cancel click did not reach the server');
}
