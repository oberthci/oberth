import {launch,check,deadline,pause} from './cdp.mjs';

export async function waitingPace() {
 const path='/runs/'+'c'.repeat(32),client=await launch({env:{OBERTH_UI_PROGRESS:'1',OBERTH_UI_LATE_LOG:'1'},artifacts:process.env.OBERTH_UI_ARTIFACTS+'/waiting-pace'});
 try {
  await client.cdp('Page.addScriptToEvaluateOnNewDocument',{source:`
   const pacedFetch=fetch;window.waitPace={times:[]};
   window.fetch=(path,options)=>{if(new URL(path,location.href).pathname.endsWith('/logs'))waitPace.times.push(performance.now());return pacedFetch(path,options);};`});
  await client.loaded(path,'.log h2');
  const post=url=>client.evaluate(`fetch(${JSON.stringify(url)},{method:'POST'}).then(response=>{if(!response.ok)throw new Error('fixture refused');})`);
  const reads=(count,label,timeout=5000)=>deadline(label,()=>client.evaluate(`new Promise(resolve=>{const test=()=>{if(waitPace.times.length>=${count})resolve([...waitPace.times]);else setTimeout(test,10);};test();})`),timeout);
  await post('/example/progress?phase=running');
  const times=await reads(7,'observe waiting backoff',20000),gaps=times.slice(1).map((at,index)=>at-times[index]);
  const expected=[250,500,1000,2000,4000,5000];
  check(gaps.length===6&&gaps.every((gap,index)=>gap>=expected[index]*.75&&gap<=expected[index]*1.3+100),'waiting log did not back off to its cap '+JSON.stringify(gaps));
  await client.evaluate(`screen.logHeader.querySelector('details').open=true`);
  const progressAt=await client.evaluate('performance.now()');
  await post('/example/progress?phase=running-progress');
  const resumed=await reads(9,'waiting log resets after progress',1500);
  check(resumed[7]-progressAt<1000&&resumed[8]-resumed[7]>=180&&resumed[8]-resumed[7]<600,'waiting log did not reset its delay '+JSON.stringify({progressAt,resumed}));
  const hidden=await client.evaluate(`(()=>{Object.defineProperty(document,'hidden',{configurable:true,get:()=>true});document.dispatchEvent(new Event('visibilitychange'));return waitPace.times.length;})()`);
  await pause(1200);
  check(await client.evaluate('waitPace.times.length')===hidden,'hidden waiting log kept polling');
  await client.evaluate(`delete document.hidden;document.dispatchEvent(new Event('visibilitychange'))`);
  await reads(hidden+1,'visible waiting log resumes',1500);
  await post('/example/log-ready');
  await deadline('paced waiting log receives output',()=>client.evaluate(`new Promise(resolve=>{const test=()=>{if(!screen.logBusy&&document.querySelector('.terminal pre').textContent.includes('prepare live output'))resolve();else setTimeout(test,10);};test();})`),7000);
  const after=await client.evaluate(`({count:waitPace.times.length,open:screen.logHeader.querySelector('details').open,notice:document.querySelector('#notice').textContent,detail:screen.logStep.detail})`);
  check(after.open&&after.notice===''&&after.detail==='Updated running diagnostics.','paced waiting log lost its panel '+JSON.stringify(after));
  await pause(600);check(await client.evaluate('waitPace.times.length')===after.count,'resolved paced log kept polling');
  client.checkFatal();return {gaps,progressAt,resumed,hidden,after};
 } finally {await client.close();}
}
