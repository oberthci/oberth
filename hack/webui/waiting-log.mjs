import {launch,check,deadline,pause} from './cdp.mjs';

export async function waitingLog() {
 const results=[],path='/runs/'+'c'.repeat(32);
 for(const finish of [false,true]) {
  const client=await launch({env:{OBERTH_UI_PROGRESS:'1',OBERTH_UI_LATE_LOG:'1'},artifacts:process.env.OBERTH_UI_ARTIFACTS+'/waiting-log-'+finish});
  try {
   await client.cdp('Page.addScriptToEvaluateOnNewDocument',{source:`
    const waitingFetch=fetch;window.waitLog={logs:0,runs:0};
    window.fetch=async(path,options)=>{
     const url=new URL(path,location.href),response=await waitingFetch(path,options);
     if(url.pathname.endsWith('/logs'))waitLog.logs++;
     if(url.pathname===${JSON.stringify('/api'+path)})waitLog.runs++;
     return response;
    };`});
   await client.loaded(path,'.log h2');
   check((await client.evaluate('waitLog.logs'))===0,'queued step read a waiting log');
   const post=url=>client.evaluate(`fetch(${JSON.stringify(url)},{method:'POST'}).then(response=>{if(!response.ok)throw new Error('fixture refused');})`);
   await post('/example/progress?phase=running');
   await deadline('running log reaches waiting',()=>client.evaluate(`new Promise(resolve=>{const test=()=>{if(screen.logBusy&&waitLog.logs>0&&document.querySelector('.log').textContent.includes('waiting for output'))resolve();else setTimeout(test,10);};test();})`));
   const before=await client.evaluate(`(()=>{screen.logHeader.querySelector('details').open=true;return {...waitLog,projection:JSON.stringify(screen.logStep)};})()`);
   await post(finish?'/example/progress?phase=passed':'/example/log-ready');
   await deadline(finish?'terminal log stops waiting':'late log appears without a projection',()=>client.evaluate(`new Promise(resolve=>{const test=()=>{if(!screen.logBusy&&${finish?"screen.runStatus==='passed'":"document.querySelector('.terminal pre').textContent.includes('prepare live output')"})resolve();else setTimeout(test,10);};test();})`));
   const inspect=()=>client.evaluate(`(()=>{const output=document.querySelector('.terminal pre'),box=output.getBoundingClientRect();return {...waitLog,projection:JSON.stringify(screen.logStep),open:screen.logHeader.querySelector('details').open,text:output.textContent,width:box.width,height:box.height,notice:document.querySelector('#notice').textContent};})()`);
   const after=await inspect();
   check(after.notice===''&&after.open&&after.logs>before.logs,'waiting log lost panel state or failed to reread '+JSON.stringify({finish,before,after}));
   if(!finish)check(after.projection===before.projection&&after.runs===before.runs&&after.width>0&&after.height>0&&after.text.includes('prepare live output'),'late log needed a step projection change '+JSON.stringify({before,after}));
   await pause(600);
   const settled=await inspect();
   check(settled.logs===after.logs,'settled or terminal log kept polling '+JSON.stringify({finish,after,settled}));
   client.checkFatal();results.push({finish,before,after,settled});
  } finally {await client.close();}
 }
 return results;
}
