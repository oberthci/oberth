import {check,deadline,pause} from './cdp.mjs';

// Hold one snapshot while events arrive, then observe the latest page after release.
export async function eventReads(client) {
 const injection=await client.cdp('Page.addScriptToEvaluateOnNewDocument',{source:`
  const eventFetch=fetch;
  const eventReply=value=>new Response(JSON.stringify(value));
  window.uiEventReads={status:'queued',message:'before events',reads:[],pending:[],events:[],cursor:0,hold:false,next:null,
   emit(status,message){this.status=status;this.message=message;this.events.push({id:++this.cursor,kind:'run.updated'});this.next?.();}
  };
  window.fetch=async(path,options={})=>{
   const url=new URL(path,location.href),state=uiEventReads;
   if(url.pathname==='/mcp'&&JSON.parse(options.body).params.name==='events_wait')return new Promise((resolve,reject)=>{
    const abort=()=>{if(state.next===send)state.next=null;reject(new DOMException('Aborted','AbortError'));};
    const send=()=>{if(!state.events.length)return;state.next=null;options.signal.removeEventListener('abort',abort);resolve(eventReply({result:{structuredContent:{events:state.events.splice(0),next_after_id:state.cursor}}}));};
    state.next=send;options.signal.addEventListener('abort',abort,{once:true});send();
   });
   if(url.pathname==='/api/runs'){
    state.page||=await (await eventFetch(path,options)).json();
    const page=structuredClone(state.page);
    Object.assign(page.runs[0],{status:state.status,display_reason:state.message});
    state.reads.push({at:performance.now(),status:state.status,message:state.message});
    const response=eventReply(page);
    if(state.hold)return new Promise(resolve=>state.pending.push(()=>resolve(response)));
    return response;
   }
   return eventFetch(path,options);
  };`});
 const wait=(label,condition)=>deadline(label,()=>client.evaluate(`new Promise(resolve=>{const test=()=>{if(${condition})resolve();else setTimeout(test,10);};test();})`));
 try {
  await client.loaded('/runs','.runs a.row');
  await wait('initial event subscription','uiEventReads.next&&!screen.reading');
  const single=await deadline('single event is read immediately',()=>client.evaluate(`(async()=>{
   const state=uiEventReads,start=performance.now(),before=state.reads.length;
   state.emit('running','first event');
   await new Promise(resolve=>{const test=()=>{const row=document.querySelector('.runs a.row');if(row.querySelector('.state').textContent.trim()!=='running'||row.querySelector('.message').textContent!=='first event')return;observer.disconnect();resolve();};const observer=new MutationObserver(test);observer.observe(main,{subtree:true,childList:true,characterData:true});test();});
   return {elapsedMS:performance.now()-start,reads:state.reads.length-before};
  })()`),750);
  check(single.reads===1,'single event made extra reads '+JSON.stringify(single));
  await wait('single event read settles','uiEventReads.next&&!screen.reading');
  await client.evaluate(`uiEventReads.burstStart=uiEventReads.reads.length;uiEventReads.hold=true;uiEventReads.emit('running','burst 0')`);
  await wait('burst first read is held','uiEventReads.pending.length===1');
  const events=8;
  for(let index=1;index<events;index++) {
   await client.evaluate(`uiEventReads.emit(${JSON.stringify(index===events-1?'passed':'running')},${JSON.stringify('burst '+index)})`);
   await pause(10);
  }
  const held=await client.evaluate(`({reads:uiEventReads.reads.length-uiEventReads.burstStart,pending:uiEventReads.pending.length})`);
  check(held.reads===1&&held.pending===1,'burst started overlapping reads '+JSON.stringify(held));
  await client.evaluate(`uiEventReads.hold=false;uiEventReads.pending.splice(0).forEach(release=>release())`);
  await wait('burst latest state is reflected',`document.querySelector('.runs a.row .state').textContent.trim()==='passed'&&document.querySelector('.runs a.row .message').textContent==='burst ${events-1}'&&!screen.reading`);
  const burst=await client.evaluate(`({reads:uiEventReads.reads.length-uiEventReads.burstStart,notice:notice.textContent})`);
  check(burst.reads<=2&&burst.notice==='','burst made more than two reads or showed an error '+JSON.stringify(burst));
  await wait('held burst subscription resumes','uiEventReads.next&&!screen.reading');
  await client.evaluate(`uiEventReads.burstStart=uiEventReads.reads.length`);
  for(let index=0;index<events;index++) {
   await client.evaluate(`uiEventReads.emit(${JSON.stringify(index===events-1?'passed':'running')},${JSON.stringify('fast burst '+index)})`);
   await pause(10);
  }
  await wait('fast burst latest state is reflected',`document.querySelector('.runs a.row .state').textContent.trim()==='passed'&&document.querySelector('.runs a.row .message').textContent==='fast burst ${events-1}'&&!screen.reading`);
  const fastBurst=await client.evaluate(`({reads:uiEventReads.reads.length-uiEventReads.burstStart,notice:notice.textContent})`);
  check(fastBurst.reads<=2&&fastBurst.notice==='','fast burst made more than two reads or showed an error '+JSON.stringify(fastBurst));
  return {single,burst:{events,...burst},fastBurst:{events,...fastBurst}};
 } finally {await client.cdp('Page.removeScriptToEvaluateOnNewDocument',{identifier:injection.identifier});}
}
