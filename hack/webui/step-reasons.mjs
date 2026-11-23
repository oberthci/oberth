import {check} from './cdp.mjs';

export async function stepReasons(client) {
 const injection=await client.cdp('Page.addScriptToEvaluateOnNewDocument',{source:`
  window.uiStepReason='passed';const reasonFetch=fetch;
  window.fetch=async(path,options)=>{
   const response=await reasonFetch(path,options);
   if(new URL(path,location.href).pathname!=='/api/runs/11111111')return response;
   const run=await response.json();run.steps=[{...run.steps[0],name:'prepare',status:'passed',display_reason:uiStepReason,detail:'Complete step detail.'}];
   return new Response(JSON.stringify(run));
  };`});
 try {
  await client.loaded('/runs/11111111','.log h2');
  const results=[];
  for(const reason of ['passed','passed','waiting for a node']) {
   if(results.length)await client.evaluate(`uiStepReason=${JSON.stringify(reason)};screen.read()`);
   const result=await client.evaluate(`({heading:document.querySelector('.log h2').textContent,reasons:Array.from(screen.logHeader.querySelectorAll(':scope > p')).map(node=>node.textContent),detail:screen.logHeader.querySelector('pre').textContent})`);
   check(result.heading==='prepare · passed'&&result.detail==='Complete step detail.'&&JSON.stringify(result.reasons)===JSON.stringify(reason==='passed'?[]:[reason]),'step panel repeats a status or loses its reason/detail '+JSON.stringify(result));
   results.push({kind:'step-reason',...result});
  }
  return results;
 } finally {await client.cdp('Page.removeScriptToEvaluateOnNewDocument',{identifier:injection.identifier});}
}
