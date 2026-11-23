import {check,deadline} from './cdp.mjs';

export async function startingRun(client,planPath='prepare') {
 const injection=await client.cdp('Page.addScriptToEvaluateOnNewDocument',{source:`
  window.uiStarting={phase:0,logs:[],reads:[]};const startingFetch=fetch;
  window.fetch=async(path,options={})=>{
   const url=new URL(path,location.href),state=uiStarting;
   if(url.pathname==='/mcp'&&JSON.parse(options.body).params.name==='events_wait')return new Promise((resolve,reject)=>{
    const abort=()=>reject(new DOMException('Aborted','AbortError'));options.signal.addEventListener('abort',abort,{once:true});
    state.next=()=>{options.signal.removeEventListener('abort',abort);resolve(new Response(JSON.stringify({result:{structuredContent:{events:[{id:state.phase}],next_after_id:state.phase}}})));};
   });
   if(url.pathname==='/api/runs/33333333'){
    const run=await (await startingFetch(path,options)).json(),status=['queued','running','passed'][state.phase];
    state.reads.push(status);
    return new Response(JSON.stringify({...run,status,display_reason:'',started_at:state.phase?run.created_at:null,finished_at:state.phase===2?run.created_at:null,passed:state.phase===2?1:0,pending:state.phase===0?1:0,step_count:1,
     steps:[{plan_path:${JSON.stringify(planPath)},name:'prepare',status,display_reason:'',attempts:state.phase?[{node_id:'prepare-one'}]:[],logs:state.phase?[{node_id:'prepare-one',container:'main'}]:[]}]}));
   }
   if(url.pathname==='/api/runs/33333333/containers')return new Response(JSON.stringify({containers:[]}));
   if(url.pathname==='/api/runs/33333333/logs'){
    state.logs.push(state.phase);
    return new Response(JSON.stringify({code:state.phase?'unavailable':'not_found',reason:state.phase?'Log read refused.':'no log matches this selector',remedy:'Read the step again.'}),{status:state.phase?403:404});
   }
   return startingFetch(path,options);
  };`});
 try {
  await client.loaded('/runs/33333333','.steps a.row');
  const queued=await client.evaluate(`({status:document.querySelector('.run-summary h1').textContent,notice:notice.textContent,logs:uiStarting.logs})`);
  check(queued.status.trim()==='queued'&&queued.notice===''&&queued.logs.length===0,'queued step read absent containers or raised a banner '+JSON.stringify(queued));
  for(const phase of [1,2]) {
   await deadline('starting run receives event wait',()=>client.evaluate(`new Promise(resolve=>{const test=()=>uiStarting.next?resolve():setTimeout(test,10);test();})`));
   await client.evaluate(`uiStarting.phase=${phase};uiStarting.next();uiStarting.next=null`);
   await deadline('starting run reaches '+['queued','running','passed'][phase],()=>client.evaluate(`new Promise(resolve=>{
    const test=()=>{if(document.querySelector('.run-summary h1').textContent.trim()!==${JSON.stringify(['queued','running','passed'][phase])})return;observer.disconnect();resolve();};
    const observer=new MutationObserver(test);observer.observe(main,{subtree:true,childList:true});test();
   })`));
  }
  const passed=await client.evaluate(`({status:document.querySelector('.steps .state').textContent.trim(),notice:notice.textContent,logError:document.querySelector('.log').textContent,logs:uiStarting.logs,reads:uiStarting.reads})`);
  check(passed.status==='passed'&&passed.notice===''&&passed.logs.includes(1)&&passed.logError.includes('Log read refused.'),'failed log read stopped live updates or became a page error '+JSON.stringify(passed));
  return {queued,passed};
 } finally {await client.cdp('Page.removeScriptToEvaluateOnNewDocument',{identifier:injection.identifier});}
}
