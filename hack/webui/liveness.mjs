import {check,deadline,pause} from './cdp.mjs';

export async function liveness(client) {
 const results=[];
 for(const [path,selector,silent] of [['/runs','.runs a.row',false],['/runs/33333333','.steps a.row',false],['/repos','.repos a.row',true]]) {
  await client.loaded(path,selector);
  if(silent)client.pauseServer(true);
  else await client.cdp('Network.emulateNetworkConditions',{offline:true,latency:0,downloadThroughput:-1,uploadThroughput:-1});
  const start=performance.now();
  try {
   await deadline('visible page notices a silent server '+path,()=>client.evaluate(`new Promise(resolve=>{
    const test=()=>{if(!document.querySelector('#notice').textContent.includes('Reconnecting to Oberth.')||document.querySelector('#server').dataset.state!=='reconnecting')return;observer.disconnect();resolve();};
    const observer=new MutationObserver(test);observer.observe(document.body,{subtree:true,childList:true,attributes:true});test();
   })`),6000);
   const down=await client.evaluate(`({notice:document.querySelector('#notice').getBoundingClientRect().height,mark:document.querySelector('#server .mark').className,main:document.querySelector('main').checkVisibility()})`);
   check(down.notice>0&&down.mark==='mark running'&&down.main,'outage did not show a notice and amber light '+JSON.stringify(down));
   results.push({path,silent,detectedMS:Math.round(performance.now()-start),...down});
  } finally {
   if(silent)client.pauseServer(false);
   else await client.cdp('Network.emulateNetworkConditions',{offline:false,latency:0,downloadThroughput:-1,uploadThroughput:-1});
  }
  await deadline('visible page recovers '+path,()=>client.evaluate(`new Promise(resolve=>{
   const test=()=>{if(document.querySelector('#notice').textContent||document.querySelector('#server').dataset.state!=='normal')return;observer.disconnect();resolve();};
   const observer=new MutationObserver(test);observer.observe(document.body,{subtree:true,childList:true,attributes:true});test();
  })`),6000);
 }
 await client.evaluate(`Object.defineProperty(document,'hidden',{configurable:true,get:()=>true});document.dispatchEvent(new Event('visibilitychange'))`);
 const before=client.requests.filter(request=>request.path==='/api/status').length;
 try {
  await pause(2100);
  check(client.requests.filter(request=>request.path==='/api/status').length===before,'hidden page sent a liveness read');
 } finally {await client.evaluate(`delete document.hidden;document.dispatchEvent(new Event('visibilitychange'))`);}
 return results;
}
