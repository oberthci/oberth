import {check} from './cdp.mjs';

export async function stepReasons(client) {
 const injection=await client.cdp('Page.addScriptToEvaluateOnNewDocument',{source:`
  window.uiStepReason='';window.uiStepStatus='pending';const reasonFetch=fetch;
  window.fetch=async(path,options)=>{
   const response=await reasonFetch(path,options);
   if(new URL(path,location.href).pathname!=='/api/runs/11111111')return response;
   const run=await response.json();run.steps=[{...run.steps[0],name:'prepare',status:uiStepStatus,display_reason:uiStepReason,detail:'Complete step detail.'}];
   return new Response(JSON.stringify(run));
  };`});
 try {
  await client.loaded('/runs/11111111','.log h2');
  const results=[];
  for(const [status,reason,specific] of [['pending','',false],['queued','',false],['running','',false],['passed','',false],['failed','',false],['skipped','',false],['unknown','execution evidence unavailable',true],['passed','waiting for a node',true],['passed','step passed with warnings',true]]) {
   if(results.length)await client.evaluate(`uiStepStatus=${JSON.stringify(status)};uiStepReason=${JSON.stringify(reason)};screen.read()`);
   const result=await client.evaluate(`({heading:document.querySelector('.log h2').textContent,headingHeight:document.querySelector('.log h2').getBoundingClientRect().height,reasons:Array.from(screen.logHeader.querySelectorAll(':scope > p')).map(node=>({text:node.textContent,height:node.getBoundingClientRect().height,width:node.getBoundingClientRect().width})),detail:screen.logHeader.querySelector('pre').textContent,summaryHeight:screen.logHeader.querySelector('summary').getBoundingClientRect().height})`);
   check(result.heading==='prepare · '+status&&result.headingHeight>0&&result.detail==='Complete step detail.'&&result.summaryHeight>0&&result.reasons.length===Number(specific)&&result.reasons.every(item=>item.text===reason&&item.height>0&&item.width>0),'status-only step reason occupies a visible line '+JSON.stringify(result));
   results.push({kind:'step-reason',...result});
  }
  return results;
 } finally {await client.cdp('Page.removeScriptToEvaluateOnNewDocument',{identifier:injection.identifier});}
}
