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
 results.push(await slowStatus(client));
 return results;
}

async function slowStatus(client) {
 const injection=await client.cdp('Page.addScriptToEvaluateOnNewDocument',{source:`
  const statusFetch=fetch;window.slowStatus={reads:0,active:0,maximum:0};
  window.fetch=async(path,options)=>{
   if(new URL(path,location.href).pathname!=='/api/status')return statusFetch(path,options);
   slowStatus.reads++;slowStatus.active++;slowStatus.maximum=Math.max(slowStatus.maximum,slowStatus.active);
   try {
    await new Promise((resolve,reject)=>{
     const abort=()=>{clearTimeout(timer);reject(options.signal.reason);};
     const timer=setTimeout(()=>{options.signal.removeEventListener('abort',abort);resolve();},1300);
     options.signal.addEventListener('abort',abort,{once:true});if(options.signal.aborted)abort();
    });
    return await statusFetch(path,options);
   } finally {slowStatus.active--;}
  };`});
 try {
  await client.loaded('/repos','.repos a.row');
  const result=await deadline('slow status remains reachable',()=>client.evaluate(`new Promise(resolve=>{
   const alerts=[],record=()=>{if(notice.textContent.includes('Reconnecting to Oberth.')&&notice.getBoundingClientRect().height>0)alerts.push(notice.getBoundingClientRect().height);};
   const observer=new MutationObserver(record);observer.observe(notice,{subtree:true,childList:true});record();
   setTimeout(()=>{observer.disconnect();resolve({...slowStatus,alerts,state:server.dataset.state,lightWidth:server.querySelector('.mark').getBoundingClientRect().width});},8000);
  })`),10000);
  check(result.reads>=3&&result.maximum===1&&result.alerts.length===0&&result.state==='normal'&&result.lightWidth>0,'slow status reply produced a visible outage '+JSON.stringify(result));
  return {kind:'slow-status',...result};
 } finally {await client.cdp('Page.removeScriptToEvaluateOnNewDocument',{identifier:injection.identifier});}
}
