import {check,deadline} from './cdp.mjs';

export async function upgrade(client) {
 const injection=await client.cdp('Page.addScriptToEvaluateOnNewDocument',{source:`
  const upgradeFetch=fetch;window.upgraded=false;
  window.fetch=async(...args)=>{
   const response=await upgradeFetch(...args);
   if(!window.upgraded)return response;
   const headers=new Headers(response.headers);headers.set('X-Oberth-UI','changed-ui');
   return new Response(response.body,{status:response.status,headers});
  };`});
 try {
  await client.loaded('/repos','.repos a.row');
  check(await client.evaluate(`document.querySelector('#upgrade').hidden`),'current UI requested a reload');
  await client.evaluate(`window.upgraded=true;readStatus()`);
  const result=await deadline('UI upgrade notice',()=>client.evaluate(`new Promise(resolve=>{
   const banner=document.querySelector('#upgrade');
   const test=()=>{if(banner.hidden)return;observer.disconnect();resolve({text:banner.textContent,visible:banner.checkVisibility(),links:banner.querySelectorAll('a').length});};
   const observer=new MutationObserver(test);observer.observe(banner,{attributes:true});test();
  })`));
  check(result.visible&&result.links===1&&result.text==='Oberth was upgraded. Reload to use the new version.','upgrade notice is incomplete '+JSON.stringify(result));
  await client.evaluate(`navigate('/runs')`);
  check(await client.evaluate(`document.querySelector('#upgrade').checkVisibility()`),'navigation hid the upgrade notice');
  const loaded=client.nextEvent('Page.loadEventFired');
  await client.evaluate(`document.querySelector('#upgrade a').click()`);await loaded;
  check(await client.evaluate(`document.querySelector('#upgrade').hidden`),'reload retained the old page warning');
  return result;
 } finally {await client.cdp('Page.removeScriptToEvaluateOnNewDocument',{identifier:injection.identifier});}
}
