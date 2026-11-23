import {writeFile} from 'node:fs/promises';
import {check} from './cdp.mjs';

export async function runHeader(client) {
 const results=[];
 for(const width of [1024,760,360]) {
  await client.cdp('Emulation.setDeviceMetricsOverride',{width,height:900,deviceScaleFactor:1,mobile:false});
  await client.loaded('/runs/33333333','.steps a.row');
  await client.evaluate(`(async()=>{screen.controller.abort();window.headerRun=await get('/api/runs/33333333');})()`);
  for(const theme of ['light','dark','mono','color'])for(const status of ['queued','running','passed','failed','cancelled']) {
   const metrics=await client.evaluate(`(()=>{
    document.documentElement.dataset.theme=${JSON.stringify(theme)};
    const run={...headerRun,status:${JSON.stringify(status)},step_count:14,passed:13,failed:0,pending:0,steps_planned_exact:true};
    if(run.status==='queued'){run.started_at=null;run.commit_sha=null;run.pending=14;run.passed=0;}
    if(run.status==='failed'){run.passed=12;run.failed=1;run.pending=1;}
    run.display_reason=['queued','running'].includes(run.status)?'waiting for an available build node soon':'';
    const header=document.querySelector('.run-head'),actions=runActions(screen,run,[],[]);
    header.replaceChildren(...runHeader(run,actions,false));
    const rect=header.getBoundingClientRect(),style=getComputedStyle(header),summary=header.querySelector('.run-summary'),meta=header.querySelector('.run-meta');
    const line=node=>{const range=document.createRange();range.selectNodeContents(node);return range.getBoundingClientRect().height;};
    return {height:rect.height-parseFloat(style.paddingTop)-parseFloat(style.paddingBottom)-parseFloat(style.borderTopWidth)-parseFloat(style.borderBottomWidth),lineHeight:parseFloat(style.lineHeight),summary:summary.textContent,counts:header.querySelector('.run-counts').textContent,
     reason:header.querySelector('.run-reason')?.textContent||'',reasonHeight:header.querySelector('.run-reason')?line(header.querySelector('.run-reason')):0,
     summaryHeight:summary.getBoundingClientRect().height,metaHeight:meta.getBoundingClientRect().height,
     sizes:[summary,meta,...summary.children].map(node=>[node.className,node.clientWidth,node.scrollWidth,getComputedStyle(node).fontSize]),overflow:document.documentElement.scrollWidth>innerWidth||summary.scrollWidth>summary.clientWidth||meta.scrollWidth>meta.clientWidth,
     cancel:Array.from(summary.querySelectorAll('button')).map(node=>({text:node.textContent,disabled:node.disabled,right:node.getBoundingClientRect().right})),right:rect.right};
   })()`);
   check(metrics.summary.startsWith(status)&&!metrics.summary.includes('run is'),'status sentence retained '+JSON.stringify(metrics));
   check(!metrics.overflow&&metrics.summaryHeight<=metrics.lineHeight&&metrics.metaHeight<=metrics.lineHeight&&metrics.height<=metrics.lineHeight*(metrics.reason?3:2),'header wraps or overflows '+JSON.stringify({width,theme,status,...metrics}));
   check(metrics.reasonHeight<=metrics.lineHeight&&(!metrics.reason||metrics.reason==='waiting for an available build node soon'),'reason cut or missing '+JSON.stringify(metrics));
   check(metrics.counts===(status==='queued'?'0/14 passed · 14 pending':status==='failed'?'12/14 passed · 1 failed · 1 pending':'13/14 passed'),'step counts changed '+JSON.stringify(metrics));
   check(['queued','running'].includes(status)?metrics.cancel.length===1&&!metrics.cancel[0].disabled&&metrics.cancel[0].right<=metrics.right:metrics.cancel.length===0,'Cancel visibility wrong '+JSON.stringify(metrics));
   const detail=await client.evaluate(`(()=>{const node=document.querySelector('.run-meta details');node.querySelector('summary').click();return {open:node.open,text:node.textContent,height:node.querySelector('pre').getBoundingClientRect().height,link:node.querySelector('a').getBoundingClientRect().height};})()`);
   check(detail.open&&detail.height>0&&(width>=1000||detail.link>0)&&detail.text.includes('Run ID:')&&detail.text.includes('feature/search')&&detail.text.includes('Other runs of this ref'),'Detail lost metadata or does not expand '+JSON.stringify(detail));
   await client.evaluate(`document.querySelector('.run-meta summary').click()`);
   if(status==='running'&&((width===1024&&theme==='light')||(width===360&&theme==='dark'))) {
    const height=await client.evaluate(`Math.ceil(document.querySelector('.run-head').getBoundingClientRect().bottom)`);
    const shot=await client.cdp('Page.captureScreenshot',{format:'png',clip:{x:0,y:0,width,height,scale:1}});
    await writeFile(`${client.artifacts}/run-header-${width}-${theme}.png`,Buffer.from(shot.data,'base64'));
   }
   results.push({width,theme,status,...metrics});
  }
 }
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
 return results;
}
