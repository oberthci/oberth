import {writeFile} from 'node:fs/promises';
import {check} from './cdp.mjs';

export async function wideCounts(client) {
 const results=[];
 for(const width of [360,760,1024]) {
  await client.cdp('Emulation.setDeviceMetricsOverride',{width,height:900,deviceScaleFactor:1,mobile:false});
  for(const theme of ['light','dark','mono','color']) {
   const result=await client.evaluate(`(async()=>{
    document.documentElement.dataset.theme=${JSON.stringify(theme)};
    const run={...headerRuns[1],step_count:140,passed:112,failed:3,pending:25,steps_planned_exact:false,started_at:new Date(Date.now()-(23*60+53)*60000).toISOString()};
    const header=document.querySelector('.run-head');header.replaceChildren(...runHeader(run,runActions(screen,run,[],[]),false));
    fitRunHeader(header.firstElementChild);
    const summary=header.querySelector('.run-summary'),counts=header.querySelector('.run-counts'),button=summary.querySelector('button'),box=button.getBoundingClientRect(),bounds=summary.getBoundingClientRect();
    const parts=Array.from(counts.children).map(node=>{const range=document.createRange();range.selectNodeContents(node);const rect=range.getBoundingClientRect();return {left:rect.left,right:rect.right,top:rect.top,bottom:rect.bottom,height:rect.height};});
    return {text:counts.textContent,duration:summary.querySelector('.duration').textContent,height:bounds.height,lineHeight:parseFloat(getComputedStyle(summary).lineHeight),button:{left:box.left,right:box.right,top:box.top,bottom:box.bottom,visible:document.elementFromPoint(box.x+box.width/2,box.y+box.height/2)===button},right:bounds.right,parts,
     overflow:document.documentElement.scrollWidth>innerWidth||summary.scrollWidth>summary.clientWidth||counts.scrollWidth>counts.clientWidth};
   })()`);
   check(result.text==='112/>=140 passed · 3 failed · 25 pending'&&result.duration==='23h 53m','wide count fixture changed '+JSON.stringify(result));
   check(!result.overflow&&result.height<=result.lineHeight*(width===360?2:1)&&result.button.visible&&result.button.right<=result.right,'wide counts overflow or hide Cancel '+JSON.stringify({width,theme,result}));
   check(result.parts.every(part=>part.height<=result.lineHeight&&(part.top>=result.button.bottom||part.bottom<=result.button.top||part.right<=result.button.left)),'counts overlap Cancel or wrap inside a fact '+JSON.stringify({width,theme,result}));
   if(width===360) {
    const height=await client.evaluate(`Math.ceil(document.querySelector('.run-head').getBoundingClientRect().bottom)`);
    const shot=await client.cdp('Page.captureScreenshot',{format:'png',clip:{x:0,y:0,width,height,scale:1}});
    await writeFile(`${client.artifacts}/run-header-wide-360-${theme}.png`,Buffer.from(shot.data,'base64'));
   }
   results.push({kind:'wide-counts',width,theme,...result});
  }
 }
 return results;
}
