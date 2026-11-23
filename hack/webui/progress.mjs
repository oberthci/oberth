import {launch,check,deadline} from './cdp.mjs';

export async function progress() {
 const results=[];
 for(const path of ['/runs','/runs/'+ 'c'.repeat(32)]) {
  const client=await launch({env:{OBERTH_UI_PROGRESS:'1'},artifacts:process.env.OBERTH_UI_ARTIFACTS+'/progress-'+(path==='/runs'?'list':'run')});
  try {
   const selector=path==='/runs'?'.runs a.row .state':'.run-summary .state';
   await client.loaded(path,selector);
   const state=()=>client.evaluate(`document.querySelector(${JSON.stringify(selector)}).textContent.trim()`);
   check(await state()==='queued','progress page did not start before the first container');
   const seen=[];
   for(const phase of ['running','steps','passed']) {
    await deadline('commit native '+phase,()=>client.evaluate(`fetch('/example/progress?phase=${phase}',{method:'POST'}).then(async response=>{if(!response.ok)throw new Error(await response.text());})`));
    const expected=phase==='passed'?'passed':'running';
    await deadline('observe native '+phase,()=>client.evaluate(`new Promise(resolve=>{
     const test=()=>{const status=document.querySelector(${JSON.stringify(selector)})?.textContent.trim();if(status!==${JSON.stringify(expected)})return;
      ${path==='/runs'?'':phase==='steps'?`if(document.querySelector('.steps [data-key="test"] .state')?.textContent.trim()!=='running')return;`:phase==='running'?`if(!document.querySelector('.terminal pre')?.textContent.includes('prepare live output'))return;`:''}
      observer.disconnect();resolve();};
     const observer=new MutationObserver(test);observer.observe(document.body,{subtree:true,childList:true,characterData:true});test();
    })`));
    const notice=await client.evaluate(`document.querySelector('#notice').textContent`);
    check(notice==='','native progress showed a page error '+notice);
    seen.push({phase,status:await state()});
   }
   results.push({path,seen});
  } finally {await client.close();}
 }
 return results;
}
