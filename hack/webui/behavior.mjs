import {check,pause,deadline} from './cdp.mjs';

async function burst(client){
 await client.loaded('/runs','.runs a.row');await pause(20);
 const result=await client.evaluate(`(async()=>{
  const original=fetch,view=screen,calls=[],reads=[],timers=[];
  let head=0,pending,active=0,maximum=0,rearmedDuringRead=false;
  const reply=request=>{const after=request.params.arguments.after_id||0,next=Math.min(head,after+(request.params.arguments.limit||100));return new Response(JSON.stringify({result:{isError:false,structuredContent:{events:Array.from({length:next-after},(_,i)=>({id:after+i+1})),next_after_id:next,has_more:next<head}}}));};
  window.fetch=(path,options)=>{
   const request=path==='/mcp'?JSON.parse(options.body):null;
   calls.push({path,at:performance.now()});
   if(!request||request.params.name!=='events_wait'){if(path.startsWith('/api/runs?'))reads.push(performance.now());return original(path,options);}
   rearmedDuringRead ||= Boolean(view.reading);active++;maximum=Math.max(maximum,active);
   return new Promise((resolve,reject)=>{
    const abort=()=>{active--;reject(new DOMException('Aborted','AbortError'));};
    const finish=()=>{options.signal.removeEventListener('abort',abort);active--;pending=null;resolve(reply(request));};
    options.signal.addEventListener('abort',abort,{once:true});
    if(head>(request.params.arguments.after_id||0))finish();else pending=finish;
   });
  };
  try{
   view.cursor=0;view.waiting.abort();
   for(let i=0;i<200;i++)timers.push(setTimeout(()=>{head=i+1;pending?.();},i*5));
   await new Promise(resolve=>setTimeout(resolve,3300));
   return {events:head,cursor:view.cursor,requests:calls.length,reads:reads.length,spacings:reads.slice(1).map((at,i)=>at-reads[i]),maximum,rearmedDuringRead};
  }finally{timers.forEach(clearTimeout);view.controller.abort();window.fetch=original;}
 })()`);
 check(result.events===200&&result.cursor===200,'burst did not consume all 200 events');
 check(result.requests<=6,'burst exceeded six requests '+JSON.stringify(result));
 check(result.maximum===1&&!result.rearmedDuringRead,'wait rearmed before its read finished');
 check(result.spacings.every(value=>value>=999.5),'view reads are less than one second apart');
 return result;
}

async function hidden(client){
 await client.loaded('/runs','.runs a.row');await pause(20);
 const hidden=await client.evaluate(`(()=>{
  const view=screen;window.uiHidden={reads:0,waits:0,aborts:0,original:fetch};
  window.fetch=(path,options)=>{if(path.startsWith('/api/runs?'))uiHidden.reads++;if(path==='/mcp'&&JSON.parse(options.body).params.name==='events_wait')uiHidden.waits++;return uiHidden.original(path,options);};
  view.waiting.signal.addEventListener('abort',()=>uiHidden.aborts++);
  Object.defineProperty(document,'hidden',{configurable:true,get:()=>true});document.dispatchEvent(new Event('visibilitychange'));
  return {aborted:view.waiting.signal.aborted};
 })()`);
 await pause(250);
 const quiet=await client.evaluate(`({reads:uiHidden.reads,waits:uiHidden.waits,aborts:uiHidden.aborts})`);
 check(hidden.aborted&&quiet.aborts===1,'hidden tab did not abort its wait');
 check(quiet.reads===0&&quiet.waits===0,'hidden tab sent requests');
 await client.evaluate(`Object.defineProperty(document,'hidden',{configurable:true,get:()=>false});document.dispatchEvent(new Event('visibilitychange'))`);
 await pause(1100);
 const visible=await client.evaluate(`(()=>{const result={reads:uiHidden.reads,waits:uiHidden.waits};screen.controller.abort();window.fetch=uiHidden.original;delete document.hidden;return result;})()`);
 check(visible.reads===1&&visible.waits===1,'visible tab did not resume with one read and wait');
 return {quiet,visible};
}

async function idleAndRecovery(client){
 await client.loaded('/runs','.runs a.row');await pause(20);
 const idle=await client.evaluate(`(async()=>{
  const original=fetch,view=screen;let waits=0,reads=0;
  window.fetch=async(path,options)=>{
   if(path.startsWith('/api/runs?'))reads++;
   if(path!=='/mcp'||JSON.parse(options.body).params.name!=='events_wait')return original(path,options);
   waits++;
   if(waits<=3){await new Promise(resolve=>setTimeout(resolve,200));return new Response(JSON.stringify({result:{isError:false,structuredContent:{events:[],next_after_id:17,timed_out:true}}}));}
   return new Promise((_,reject)=>options.signal.addEventListener('abort',()=>reject(new DOMException('Aborted','AbortError')),{once:true}));
  };
  try{view.waiting.abort();await new Promise(resolve=>setTimeout(resolve,1400));return {reads,waits,cursor:view.cursor};}
  finally{view.controller.abort();window.fetch=original;}
 })()`);
 check(idle.reads===0&&idle.waits===4&&idle.cursor===17,'empty timeouts triggered a read or lost the cursor');
 await client.loaded('/runs','.runs a.row');await pause(20);
 await client.evaluate(`(()=>{
  const original=fetch,view=screen;window.uiRecovery={original,waits:0};
  window.fetch=async(path,options)=>{
   if(path.startsWith('/api/runs?'))return new Response(JSON.stringify({reason:'Read unavailable.',remedy:'Retry the read.'}),{status:503});
   if(path!=='/mcp')return original(path,options);
   uiRecovery.waits++;
   if(uiRecovery.waits===1)return new Response(JSON.stringify({reason:'Live refused.',remedy:'Retry the wait.'}),{status:403,headers:{'Retry-After':'1'}});
   if(uiRecovery.waits===2)return new Response(JSON.stringify({result:{isError:false,structuredContent:{events:[],next_after_id:18}}}));
   return new Promise((_,reject)=>options.signal.addEventListener('abort',()=>reject(new DOMException('Aborted','AbortError')),{once:true}));
  };
  view.waiting.abort();
 })()`);
 await pause(100);
 const refused=await client.evaluate(`notice.textContent`);
 // A refusal with a reason is shown at once; an unavailable read is connection trouble and stays quiet.
 check(refused.includes('Live refused.')&&refused.includes('Live updates stopped.')&&!refused.includes('Read unavailable.'),'live refusal not shown, or a 503 read was '+refused);
 await pause(1200);
 const recovered=await client.evaluate(`(()=>{const result={notice:notice.textContent,waits:uiRecovery.waits};screen.controller.abort();window.fetch=uiRecovery.original;return result;})()`);
 check(recovered.waits===3&&recovered.notice==='','recovered observation retained a stale error');
 return {idle,recovered};
}

async function actions(client,label,path,tool){
 await client.loaded(path,'.steps a.row');await pause(20);
 await client.evaluate(`(()=>{
  const original=fetch,confirm=window.confirm;window.uiAction={original,confirm,calls:0,waiting:0,label:${JSON.stringify(label)}};
  window.confirm=()=>true;const read=screen.read;screen.read=async()=>{await read();await new Promise(resolve=>uiAction.finishRead=resolve);};
  window.fetch=(path,options)=>{
   if(path==='/mcp'&&JSON.parse(options.body).params.name===${JSON.stringify(tool)})uiAction.calls++;
   if(path.startsWith('/api/runs/')&&path.includes('view=dashboard')&&uiAction.calls){uiAction.waiting++;return new Promise(resolve=>uiAction.release=()=>resolve(original(path,options)));}
   return original(path,options);
  };
  uiAction.button=Array.from(document.querySelectorAll('main button')).find(node=>node.textContent===uiAction.label);uiAction.button.click();
 })()`);
 await pause(250);
 const second=await client.evaluate(`(()=>{const disabled=uiAction.button.disabled;Array.from(document.querySelectorAll('main button')).find(node=>node.textContent===uiAction.label).click();return {disabled,calls:uiAction.calls};})()`);
 check(second.disabled&&second.calls===1,tool+' is not double-click safe at 250 ms');
 await pause(1000);
 const blocked=await client.evaluate(`({waiting:uiAction.waiting,disabled:Array.from(document.querySelectorAll('main button')).find(node=>node.textContent===uiAction.label).disabled})`);
 check(blocked.waiting===1&&blocked.disabled,tool+' enabled before its reread completed');
 await client.evaluate(`uiAction.release()`);await pause(150);
 const rebuilt=await client.evaluate(`(()=>{const button=Array.from(document.querySelectorAll('main button')).find(node=>node.textContent===uiAction.label);button.click();return {different:button!==uiAction.button,disabled:button.disabled,reading:Boolean(screen.reading),calls:uiAction.calls,atEnd:Boolean(uiAction.finishRead)};})()`);
 check(rebuilt.atEnd&&rebuilt.different&&rebuilt.reading&&rebuilt.disabled&&rebuilt.calls===1,tool+' rebuilt an enabled button before its reread finished');
 await client.evaluate(`uiAction.finishRead()`);await pause(150);
 const refreshed=await client.evaluate(`(()=>{const result={calls:uiAction.calls,oldDisabled:uiAction.button.disabled,newDisabled:Array.from(document.querySelectorAll('main button')).find(node=>node.textContent===uiAction.label).disabled};window.fetch=uiAction.original;window.confirm=uiAction.confirm;return result;})()`);
 check(refreshed.calls===1&&refreshed.oldDisabled&&!refreshed.newDisabled,tool+' did not release only the fresh toolbar');
 return {second,blocked,rebuilt,refreshed};
}

// Connection trouble never fills the page: the header light turns amber after two
// failed requests in a row, red with a reload link after 30 s, and back when the server answers.
async function offline(client){
 await client.loaded('/runs','.runs a.row');await pause(20);
 const result=await client.evaluate(`(async()=>{
  const original=fetch,clock=performance.now,view=screen,theme=document.querySelector('#theme');let shift=0,reads=0,waits=0,notices=0;
  const observer=new MutationObserver(()=>{if(notice.textContent)notices++;});observer.observe(notice,{subtree:true,childList:true,characterData:true});
  const look=()=>({state:server.dataset.state,mark:server.querySelector('.mark').className,title:server.title,label:server.getAttribute('aria-label'),reload:getComputedStyle(server.querySelector('.reload')).visibility,version:getComputedStyle(server.querySelector('.ver')).visibility,theme:theme.getBoundingClientRect().left,width:server.getBoundingClientRect().width});
  const before=look();
  window.fetch=async path=>{if(path==='/mcp')waits++;else reads++;throw Object.assign(new TypeError('offline'),{retry:25});};
  performance.now=()=>clock.call(performance)+shift;
  try{
   view.lastRead=-Infinity;view.waiting.abort();
   await new Promise(resolve=>setTimeout(resolve,400));
   const reconnecting=look();
   shift=31000;await new Promise(resolve=>setTimeout(resolve,1300));
   const unreachable=look();
   window.fetch=original;await new Promise(resolve=>setTimeout(resolve,1500));
   return {before,reconnecting,unreachable,after:look(),reads,waits,notices,notice:notice.textContent};
  }finally{view.controller.abort();window.fetch=original;performance.now=clock;observer.disconnect();}
 })()`);
 check(result.reads>=2&&result.waits>=2,'offline check missed the read/wait retry cycle '+JSON.stringify(result));
 check(result.notices===0&&result.notice==='','connection trouble filled the page '+JSON.stringify(result));
 check(result.before.state==='normal'&&result.before.mark==='mark passed'&&result.before.reload==='hidden','header did not start normal '+JSON.stringify(result.before));
 check(result.reconnecting.state==='reconnecting'&&result.reconnecting.mark==='mark running'&&result.reconnecting.title.startsWith('Reconnecting to Oberth.')&&result.reconnecting.reload==='hidden','two failures did not show reconnecting '+JSON.stringify(result.reconnecting));
 check(result.unreachable.state==='unreachable'&&result.unreachable.mark==='mark failed'&&result.unreachable.title.includes('reload this page')&&result.unreachable.label.startsWith('Oberth is not reachable.')&&result.unreachable.reload==='visible'&&result.unreachable.version==='hidden','30 s of failures did not show unreachable with reload '+JSON.stringify(result.unreachable));
 check(result.after.state==='normal'&&result.after.mark==='mark passed'&&result.after.reload==='hidden','header did not recover by itself '+JSON.stringify(result.after));
 check([result.reconnecting,result.unreachable,result.after].every(state=>state.theme===result.before.theme&&state.width===result.before.width),'header moved when the connection changed '+JSON.stringify(result));
 return result;
}

async function confirmationAndFetch(client){
 await client.loaded('/runs/33333333','.steps a.row');await pause(20);
 const result=await client.evaluate(`(async()=>{
  const original=fetch,confirmOriginal=confirm;let calls=0,confirmations=0;
  const options=uiFetches.filter(item=>item.path.startsWith('/api/')||item.path==='/mcp');
  window.fetch=(path,opts)=>{if(path==='/mcp'&&JSON.parse(opts.body).params.name==='run_cancel')calls++;return original(path,opts);};
  window.confirm=()=>{confirmations++;return false;};
  try{Array.from(document.querySelectorAll('main button')).find(node=>node.textContent==='Cancel').click();await new Promise(resolve=>setTimeout(resolve,100));return {calls,confirmations,options};}
  finally{window.fetch=original;window.confirm=confirmOriginal;}
 })()`);
 check(result.confirmations===1&&result.calls===0,'cancel bypassed its declined confirmation');
 check(result.options.length>=3&&result.options.every(item=>item.credentials==='same-origin'&&item.redirect==='error'&&item.authorization),'fetch credentials or redirect settings changed');
 return result;
}

async function refusedRerun(client){
 await client.loaded('/runs/77777777','.steps a.row');await pause(20);
 const findings='refs/heads/topic .oberth/build.yaml spec.templates["build"].container.image: <script>window.uiInjected=1</script> image is outside the allowed registries.\n  fix: Use an image under an allowed prefix.';
 const result=await client.evaluate(`(async()=>{
  const original=fetch;
  window.fetch=(path,options)=>{
   if(path==='/mcp'&&JSON.parse(options.body).params.name==='run_rerun'){
    const refusal={code:'invalid_input',reason:'pipeline admission failed',remedy:'Fix every reported finding, then push again.',context:[${JSON.stringify(findings)}]};
    return Promise.resolve(new Response(JSON.stringify({jsonrpc:'2.0',id:1,result:{isError:true,content:[{type:'text',text:JSON.stringify(refusal)}]}}),{headers:{'Content-Type':'application/json'}}));
   }
   return original(path,options);
  };
  try{
   Array.from(document.querySelectorAll('main button')).find(node=>node.textContent==='Rerun').click();
   await new Promise(resolve=>setTimeout(resolve,150));
   return {text:notice.textContent,listed:Array.from(notice.querySelectorAll('pre')).map(node=>node.textContent),scripts:notice.querySelectorAll('script').length,injected:window.uiInjected===1};
  }finally{window.fetch=original;}
 })()`);
 check(result.text.includes('pipeline admission failed')&&result.listed.length===1&&result.listed[0]===findings,'refused rerun hid its findings '+JSON.stringify(result));
 check(result.scripts===0&&!result.injected,'refused rerun findings were interpreted as markup');
 return result;
}

async function logAppend(client){
 const result=await client.evaluate(`(()=>{
  const regions={output:h('pre'),status:h('div')};resetLog(regions,'x'.repeat(1048576));
  const original=TextEncoder.prototype.encode;let largest=0,encoded=0;
  TextEncoder.prototype.encode=function(text){const bytes=original.call(this,text);largest=Math.max(largest,bytes.length);encoded+=bytes.length;return bytes;};
  try{for(let i=0;i<128;i++)appendLive(regions,'y');}
  finally{TextEncoder.prototype.encode=original;}
  return {largest,encoded,bytes:original.call(new TextEncoder(),regions.output.textContent).length,nodes:regions.output.childNodes.length,ends:regions.output.textContent.endsWith('y'.repeat(128))};
 })()`);
 check(result.bytes===1048576&&result.ends,'live tail is not complete at its byte bound');
 check(result.largest<=4096&&result.encoded<=4096*128+128&&result.nodes<=260,'live append reprocessed the retained buffer');
 return result;
}

async function signout(client){
 await client.loaded('/runs','.runs a.row');
 const headerState=()=>client.evaluate(`({nav:document.querySelector('nav').getBoundingClientRect().height>0,signout:document.querySelector('#signout').getBoundingClientRect().height>0,header:document.querySelector('header').getBoundingClientRect().height>0})`);
 const signedIn=await headerState();
 check(signedIn.nav&&signedIn.signout&&signedIn.header,'signed-in navigation or Sign out missing');
 await deadline('load after sign-out',async()=>{const ready=client.nextEvent('Page.loadEventFired');await client.evaluate(`document.querySelector('#signout').click()`);await ready;});await pause(100);
 const result=await client.evaluate(`({hasToken:localStorage.getItem('oberth.token')!==null,form:!signin.hidden,mainHidden:main.hidden,notice:notice.textContent})`);
 check(!result.hasToken,'sign-out retained the bearer token');
 check(result.form&&result.mainHidden&&result.notice==='', 'first visit did not show a quiet sign-in form');
 const signedOut=await headerState();
 check(!signedOut.nav&&!signedOut.signout&&!signedOut.header,'signed-out page shows navigation or Sign out');
 await client.evaluate(`localStorage.setItem('oberth.token','invalid');route()`);
 await pause(100);
 check((await client.evaluate(`notice.textContent`)).includes('credentials'), 'invalid credentials did not show their refusal');
 const refused=await headerState();
 check(!refused.nav&&!refused.signout&&!refused.header,'refused sign-in shows navigation or Sign out');
 await deadline('load after sign-in',async()=>{
  const ready=client.nextEvent('Page.loadEventFired');
  await client.evaluate(`signin.querySelector('input').value=${JSON.stringify(client.token)};signin.querySelector('form').requestSubmit()`);
  await ready;
  await client.evaluate(`new Promise(resolve=>{const test=()=>{if(document.querySelector('header').hidden||!document.querySelector('.runs a.row'))return;observer.disconnect();resolve();};const observer=new MutationObserver(test);observer.observe(document,{subtree:true,childList:true,attributes:true});test();})`);
 });
 const restored=await headerState();
 check(restored.nav&&restored.signout&&restored.header,'sign-in did not restore navigation and Sign out');
 return {...result,signedIn,signedOut,refused,restored};
}

async function inspectHolds(client){
 await client.loaded('/runs/33333333','.steps a.row');
 const known=await client.evaluate(`document.querySelector('main').textContent`);
 check(known.includes('held since ')&&known.includes('about 600s left')&&known.includes('Release'),'live hold timing missing');
 check(!known.includes('Continue'),'removed continue action visible');
 await client.loaded('/runs/22222222','.steps a.row');
 const unknown=await client.evaluate(`document.querySelector('main').textContent`);
 check(unknown.includes('availability unknown')&&unknown.includes('Release'),'reachable hold hidden by native failure');
 check(!unknown.includes('about 600s left')&&!unknown.includes('held since '),'unknown availability guessed timing');
 const failed=await client.evaluate(`(async()=>{
 const original=fetch;
 window.fetch=(path,options)=>path==='/mcp'&&JSON.parse(options.body).params.name==='run_hold_list'?Promise.reject(new Error('native unavailable')):original(path,options);
 try { await screen.read();return {text:main.textContent,steps:document.querySelectorAll('.steps a.row').length}; }
 finally { window.fetch=original; }
 })()`);
 check(failed.text.includes('hold availability unknown')&&failed.steps>0,'hold failure hid the run page');
 return {known:true,unknown:true,failedList:true};
}

async function nativeIssues(client){
 const results=[];
 for(const [id,summary] of [[45,'manual · 1 occurrence'],[46,'manual · 2 occurrences']]){
  await client.loaded('/issues/'+id,'.issue h1');
  const result=await client.evaluate(`(()=>{
   const region=document.querySelector('main > .issue');
   return {summary:Array.from(region.querySelectorAll('p')).find(node=>node.textContent.startsWith('manual · '))?.textContent,
    body:region.querySelector('pre').textContent,imported:Boolean(region.querySelector('.imported-source')),
    strayNull:Array.from(region.childNodes).some(node=>node.nodeType===Node.TEXT_NODE&&node.textContent==='null')};
  })()`);
  check(result.summary===summary,'native issue occurrence count is not grammatical');
  check(!result.imported&&!result.strayNull,'native issue acquired imported-source or null text');
  check(result.body==='null','authored null text was hidden');
  results.push(result);
 }
 return results;
}

async function importedIssue(client){
 await client.loaded('/issues/42','.imported-source ol li');
 const result=await client.evaluate(`(()=>{
  const source=document.querySelector('.imported-source');
  return {text:source.textContent,entries:Array.from(source.querySelectorAll('ol li')).map(node=>node.textContent),injected:source.querySelectorAll('script').length};
 })()`);
 check(result.text.includes('Imported source')&&result.text.includes('Opened by source author')&&result.text.includes('Original closer: source closer'),'source attribution missing');
 check(result.text.includes('2026-11-16T14:00:00Z')&&result.text.includes('2026-11-16T13:00:00Z'),'source timestamps missing');
 check(result.entries.length===2&&result.entries[0].includes('first source editor')&&result.entries[0].includes('First continued text <script>plain source</script>')&&result.entries[1].includes('second source editor')&&result.entries[1].includes('Second source entry'),'source history order or text changed');
 check(result.injected===0,'source history was interpreted as markup');
 await client.loaded('/issues/43','.imported-source');
 const open=await client.evaluate(`document.querySelector('.imported-source').textContent`);
 check(open.includes('Opened by source author')&&!open.includes('Original closer:'),'never-closed source acquired a closer');
 await client.loaded('/issues/44','.imported-source');
 const unknown=await client.evaluate(`document.querySelector('.imported-source').textContent`);
 check(unknown.includes('Original closer: unknown'),'closed source lost its unknown-closer attribution');
 return {...result,open,unknown};
}

// The issue workspace: chips with counts, paging both ways, search, stale rows and the way back.
const issueState=`({search:location.search,chips:Array.from(document.querySelectorAll('.chips a')).map(link=>[link.textContent,link.getAttribute('aria-current')]),positions:Array.from(document.querySelectorAll('.pages .position')).map(node=>node.textContent),newer:Array.from(document.querySelectorAll('.pages')).map(pages=>pages.children[1].tagName),older:Array.from(document.querySelectorAll('.pages')).map(pages=>pages.children[2].tagName),rows:Array.from(document.querySelectorAll('.issues a.row')).map(row=>row.dataset.key)})`;
const issueRead=(client,mark)=>client.requests.slice(mark).filter(request=>request.path==='/api/issues').map(request=>Object.fromEntries(new URL(request.url).searchParams));

async function issueChipsAndPaging(client){
 await client.loaded('/issues','.issues a.row');
 const first=await client.evaluate(issueState);
 check(JSON.stringify(first.chips)===JSON.stringify([['all open 64','true'],['todo 40',null],['active 3',null],['waiting 7',null],['review 14',null],['stale 4',null],['closed 8',null]]),'chips lack their counts or the current mark '+JSON.stringify(first.chips));
 check(JSON.stringify(first.positions)==='["1-50 of 64","1-50 of 64"]'&&first.rows.length===50&&first.newer.join()==='SPAN,SPAN'&&first.older.join()==='A,A','first page position or pager wrong '+JSON.stringify(first));
 let mark=client.requests.length;
 await client.evaluate(`document.body.dispatchEvent(new KeyboardEvent('keydown',{key:'ArrowRight',bubbles:true}))`);
 await settle(client,`new URLSearchParams(location.search).has('before')&&document.querySelector('.pages .position')?.textContent==='51-64 of 64'`,'older page');
 const older=await client.evaluate(issueState);
 check(older.rows.length===14&&older.newer.join()==='A,A'&&older.older.join()==='SPAN,SPAN'&&!older.rows.some(id=>first.rows.includes(id)),'older page overlaps or offers no way back '+JSON.stringify(older));
 check(issueRead(client,mark).some(query=>query.before===first.rows.at(-1)&&query.limit==='50'),'older page did not use the before cursor');
 mark=client.requests.length;
 await client.evaluate(`Array.from(document.querySelectorAll('.pages'))[1].querySelector('a').click()`);
 await settle(client,`new URLSearchParams(location.search).has('after')&&document.querySelector('.pages .position')?.textContent==='1-50 of 64'`,'newer page');
 const newer=await client.evaluate(issueState);
 check(JSON.stringify(newer.rows)===JSON.stringify(first.rows)&&newer.newer.join()==='SPAN,SPAN','newer page did not return to the first page '+JSON.stringify(newer));
 check(issueRead(client,mark).some(query=>query.after===older.rows[0]),'newer page did not use the after cursor');
 mark=client.requests.length;
 await client.evaluate(`Array.from(document.querySelectorAll('.chips a')).find(link=>link.textContent.startsWith('active')).click()`);
 await settle(client,`location.search==='?work_status=active'&&document.querySelectorAll('.issues a.row').length===3`,'active chip');
 const active=await client.evaluate(issueState);
 check(JSON.stringify(active.chips.filter(chip=>chip[1]))==='[["active 3","true"]]'&&active.positions[0]==='1-3 of 3'&&JSON.stringify(active.rows)==='["71","70","42"]','active chip did not filter '+JSON.stringify(active));
 check(issueRead(client,mark).some(query=>query.work_status==='active'&&!query.before&&!query.after),'active chip did not reach issue_list');
 return {first,older,newer,active};
}

async function issueSearchAndStale(client){
 await client.loaded('/issues?stale=true','.issues a.row');
 const stale=await client.evaluate(`Array.from(document.querySelectorAll('.issues a.row')).map(row=>({id:row.dataset.key,touched:row.querySelector('.touched').textContent,stale:row.querySelector('.touched').classList.contains('stale'),colour:getComputedStyle(row.querySelector('.touched')).color,lock:row.querySelector('.lock')?.textContent||null,place:[row.querySelector('.place').textContent,row.querySelector('.place').title]}))`);
 check(JSON.stringify(stale.map(row=>[row.id,row.touched]))===JSON.stringify([['70','stale · 3h untouched'],['68','stale · 26h untouched'],['42','stale · 10h untouched'],['1','stale · 24h untouched']])&&stale.every(row=>row.stale),'stale rows lack their reason line '+JSON.stringify(stale));
 const running=await client.evaluate(`(()=>{const probe=h('span',{class:'running'});document.body.append(probe);const colour=getComputedStyle(probe).color;probe.remove();return colour;})()`);
 check(stale.every(row=>row.colour===running),'stale text is not in the warning token '+JSON.stringify({running,stale}));
 check(JSON.stringify(stale[2].lock)==='"editor · 5m left"'&&JSON.stringify(stale.map(row=>row.place))==='[["api","origin/team/api"],["origin","origin"],["cli","mirror/tools/cli"],["web","origin/team/web"]]','lock line or place wrong '+JSON.stringify(stale));
 await client.loaded('/issues','.issues a.row');
 const locked=await client.evaluate(`document.querySelector('.issues a.row[data-key="71"] .lock').textContent`);
 check(locked==='agent-b · 3m left','lock holder and time left missing '+locked);
 const mark=client.requests.length;
 await client.evaluate(`(()=>{const input=document.querySelector('.issue-search');input.focus();input.value='fla';input.dispatchEvent(new Event('input'));input.value='flaky';input.dispatchEvent(new Event('input'));})()`);
 await settle(client,`location.search==='?query=flaky'&&document.querySelectorAll('.issues a.row').length===9`,'search');
 const searched=await client.evaluate(`({focused:document.activeElement===document.querySelector('.issue-search'),value:document.querySelector('.issue-search').value,chips:Array.from(document.querySelectorAll('.chips a')).map(link=>[link.textContent,new URL(link.href).search]),titles:Array.from(document.querySelectorAll('.issues .title')).every(node=>/flaky/i.test(node.textContent))})`);
 const reads=issueRead(client,mark);
 check(searched.focused&&searched.value==='flaky'&&searched.titles,'search lost focus or matched other titles '+JSON.stringify(searched));
 check(reads.length===1&&reads[0].query==='flaky','search was not debounced to one read '+JSON.stringify(reads));
 check(JSON.stringify(searched.chips.slice(0,3))===JSON.stringify([['all open 9','?query=flaky'],['todo 8','?query=flaky&work_status=todo'],['active 1','?query=flaky&work_status=active']]),'chips do not count or keep the search '+JSON.stringify(searched.chips));
 return {stale,searched};
}

async function issueBackLink(client){
 await client.loaded('/issues?work_status=active','.issues a.row');
 await client.evaluate(`document.querySelector('.issues a.row[data-key="70"]').click()`);
 await settle(client,`location.pathname==='/issues/70'&&Boolean(document.querySelector('.issue h1'))`,'issue page');
 const detail=await client.evaluate(`({search:location.search,back:document.querySelector('main > a.back').getAttribute('href'),title:document.querySelector('.issue h1').textContent,states:Array.from(document.querySelectorAll('.issue-states .state')).map(node=>node.textContent),touched:document.querySelector('.issue-states .touched').textContent,body:document.querySelector('.issue-body').textContent})`);
 check(detail.search==='?work_status=active'&&detail.back==='/issues?work_status=active'&&detail.title.startsWith('#70 ')&&JSON.stringify(detail.states)==='["open","active"]'&&detail.touched==='stale · 3h untouched'&&detail.body.includes('failing command'),'issue page lost the list or its states '+JSON.stringify(detail));
 await client.evaluate(`document.querySelector('main > a.back').click()`);
 await settle(client,`location.search==='?work_status=active'&&document.querySelectorAll('.issues a.row').length===3`,'back link');
 await client.evaluate(`document.querySelector('.issues a.row[data-key="42"]').click()`);
 await settle(client,`location.pathname==='/issues/42'&&Boolean(document.querySelector('.issue h1'))`,'second issue');
 await client.evaluate(`document.body.dispatchEvent(new KeyboardEvent('keydown',{key:'Escape',bubbles:true}))`);
 await settle(client,`location.pathname==='/issues'&&location.search==='?work_status=active'&&document.querySelector('.chips a[aria-current="true"]')?.textContent==='active 3'`,'escape back to the list');
 return detail;
}

async function settle(client,test,label){
 for(let i=0;i<150;i++){if(await client.evaluate(test))return;await pause(20);}
 check(false,label+' did not settle');
}

const sideLinks=`Array.from(document.querySelectorAll('.repo-side a')).map(link=>[link.getAttribute('aria-label')||link.textContent,link.getAttribute('aria-current')])`;
const clickRepo=name=>`document.querySelector('.repo-side a[aria-label=${JSON.stringify(name)}]').click()`;

async function repositoryListAndChips(client){
 await client.loaded('/runs','.runs a.row');await pause(20);
 const mark=client.requests.length;
 await client.evaluate(clickRepo('origin/team/web'));
 await settle(client,`location.search==='?repo=origin%2Fteam%2Fweb'&&Boolean(document.querySelector('.runs a.row'))`,'repository choice');
 const side=await client.evaluate(sideLinks);
 check(JSON.stringify(side)===JSON.stringify([['All repos',null],['mirror/tools/cli',null],['origin/team/api',null],['origin/team/web','true']]),'chosen repository not marked '+JSON.stringify(side));
 check(client.requests.slice(mark).some(request=>request.path==='/api/runs'&&new URL(request.url).searchParams.get('repo')==='origin/team/web'),'repository list did not filter the run list');
 const chipMark=client.requests.length;
 await client.evaluate(`Array.from(document.querySelectorAll('.chips a')).find(link=>link.textContent==='failed').click()`);
 await settle(client,`new URLSearchParams(location.search).get('status')==='failed'&&Boolean(document.querySelector('.runs a.row'))`,'status chip');
 const chips=await client.evaluate(`({search:location.search,current:Array.from(document.querySelectorAll('.chips a[aria-current="true"]')).map(link=>link.textContent),side:document.querySelector('.repo-side a[aria-current="true"]').getAttribute('aria-label'),title:document.querySelector('.repo-side a[aria-current="true"]').title})`);
 check(chips.search==='?repo=origin%2Fteam%2Fweb&status=failed'&&JSON.stringify(chips.current)==='["failed"]'&&chips.side==='origin/team/web'&&chips.title==='origin/team/web. Select it again to show all repositories.','chip choice lost the address or the repository '+JSON.stringify(chips));
 check(client.requests.slice(chipMark).some(request=>{const query=new URL(request.url).searchParams;return request.path==='/api/runs'&&query.get('status')==='failed'&&query.get('repo')==='origin/team/web';}),'status chip did not filter the run list');
 // Clearing the repository: select it again, press Escape, or choose All repos; the status stays.
 await client.evaluate(clickRepo('origin/team/web'));
 await settle(client,`location.search==='?status=failed'&&document.querySelector('.repo-side a[aria-current="true"]')?.textContent==='All repos'`,'selecting the chosen repository again');
 await client.evaluate(clickRepo('origin/team/api'));
 await settle(client,`location.search==='?status=failed&repo=origin%2Fteam%2Fapi'`,'second repository');
 await client.evaluate(`document.body.dispatchEvent(new KeyboardEvent('keydown',{key:'Escape',bubbles:true}))`);
 await settle(client,`location.search==='?status=failed'`,'Escape');
 await client.evaluate(clickRepo('mirror/tools/cli'));
 await settle(client,`location.search==='?status=failed&repo=mirror%2Ftools%2Fcli'`,'third repository');
 await client.evaluate(`document.querySelector('.repo-side a.repo-group').click()`);
 await settle(client,`location.search==='?status=failed'`,'All repos');
 await client.evaluate(`Array.from(document.querySelectorAll('.chips a')).find(link=>link.textContent==='all').click()`);
 await settle(client,`location.search===''`,'all chip');
 return {side,chips};
}

// Group headers are disclosures: collapsing hides their rows (not their runs), the choice
// survives a reload through storage, the chosen repository's group stays open, and the list
// still works when storage throws.
async function sideGroups(client){
 const groups=`Array.from(document.querySelectorAll('.repo-side button.repo-group')).map(button=>[button.textContent,button.getAttribute('aria-expanded'),document.getElementById(button.getAttribute('aria-controls')).hidden])`;
 await client.loaded('/runs','.runs a.row');await pause(20);
 await client.evaluate(`localStorage.removeItem('oberth.collapsed')`);
 await client.loaded('/runs','.runs a.row');await pause(20);
 const mark=client.requests.length;
 await client.evaluate(`document.querySelector('.repo-side button.repo-group').click()`);
 const collapsed=await client.evaluate(`({groups:${groups},search:location.search,runs:document.querySelectorAll('.runs a.row').length,stored:localStorage.getItem('oberth.collapsed')})`);
 check(JSON.stringify(collapsed.groups)===JSON.stringify([['mirror/tools','false',true],['origin/team','true',false]])&&collapsed.search===''&&collapsed.runs===4&&collapsed.stored==='["mirror/tools"]','collapsing a group filtered runs or was not stored '+JSON.stringify(collapsed));
 check(!client.requests.slice(mark).some(request=>request.path==='/api/runs'),'collapsing a group read the runs again');
 await client.loaded('/runs','.runs a.row');await pause(20);
 const reloaded=await client.evaluate(groups);
 check(JSON.stringify(reloaded)===JSON.stringify([['mirror/tools','false',true],['origin/team','true',false]]),'collapsed group did not survive a reload '+JSON.stringify(reloaded));
 await client.loaded('/runs?repo=mirror%2Ftools%2Fcli','.runs a.row');await pause(20);
 const chosen=await client.evaluate(groups);
 check(chosen[0][1]==='true'&&!chosen[0][2],'the chosen repository was hidden in a collapsed group '+JSON.stringify(chosen));
 await client.loaded('/runs','.runs a.row');await pause(20);
 await client.evaluate(`document.querySelector('.repo-side button.repo-group').click()`);
 const reopened=await client.evaluate(`({groups:${groups},stored:localStorage.getItem('oberth.collapsed')})`);
 check(reopened.groups[0][1]==='true'&&reopened.stored==='[]','expanding a group was not stored '+JSON.stringify(reopened));
 const broken=await client.evaluate(`(async()=>{
  const get=Storage.prototype.getItem,set=Storage.prototype.setItem;
  Storage.prototype.getItem=function(key){if(key==='oberth.collapsed')throw new DOMException('denied','SecurityError');return get.call(this,key);};
  Storage.prototype.setItem=function(key,value){if(key==='oberth.collapsed')throw new DOMException('denied','SecurityError');return set.call(this,key,value);};
  try{await route();document.querySelector('.repo-side button.repo-group').click();return {groups:${groups},rows:document.querySelectorAll('.runs a.row').length,notice:notice.textContent};}
  finally{Storage.prototype.getItem=get;Storage.prototype.setItem=set;screen.controller.abort();}
 })()`);
 check(broken.groups[0][1]==='false'&&broken.groups[1][1]==='true'&&broken.rows===4&&broken.notice==='','the list failed without storage '+JSON.stringify(broken));
 return {collapsed,reloaded,chosen,broken};
}

async function shortNameCollisions(client){
 await client.loaded('/runs','.runs a.row');await pause(20);
 const all=await client.evaluate(`(async()=>{
  const original=fetch,repos=['origin/team/web','origin/team/api','mirror/tools/cli','mirror/tools/api','mirror/team/web'];
  window.fetch=(path,options)=>path.startsWith('/api/repos?')?Promise.resolve(new Response(JSON.stringify({repositories:repos.map(repo=>({repo}))}))):original(path,options);
  try{await route();
   return {links:Array.from(document.querySelectorAll('.repo-side a')).map(link=>[link.textContent,link.getAttribute('aria-label')]),groups:Array.from(document.querySelectorAll('.repo-side button.repo-group')).map(node=>node.textContent),options:Array.from(document.querySelectorAll('.filters optgroup')).map(group=>[group.label,Array.from(group.children).map(option=>option.textContent)])};}
  finally{screen.controller.abort();window.fetch=original;}
 })()`);
 check(JSON.stringify(all.links)===JSON.stringify([['All repos',null],['mirror/team/web','mirror/team/web'],['tools/api','mirror/tools/api'],['cli','mirror/tools/cli'],['team/api','origin/team/api'],['origin/team/web','origin/team/web']])&&JSON.stringify(all.groups)===JSON.stringify(['mirror/team','mirror/tools','origin/team']),'colliding short names were not told apart '+JSON.stringify(all));
 check(JSON.stringify(all.options)===JSON.stringify([['mirror/team',['mirror/team/web']],['mirror/tools',['tools/api','cli']],['origin/team',['team/api','origin/team/web']]]),'repository select differs from the list '+JSON.stringify(all.options));
 return all;
}

// A long repository or ref name ends in an ellipsis with its whole value as the title;
// branch refs lose refs/heads/, tags read "tag <name>", and the row stays one line.
async function runCells(client){
 await client.loaded('/runs','.runs a.row');await pause(20);
 const result=await client.evaluate(`(async()=>{
  const original=fetch,long='origin/team/'+'a-very-long-repository-name'.repeat(2),run=(id,repo,ref,status)=>({id:id.repeat(32/id.length),repo,ref,status,created_at:'2026-11-16T14:00:00Z',started_at:'2026-11-16T14:00:00Z',finished_at:'2026-11-16T14:04:00Z',object_sha:'1234',display_reason:''});
  window.fetch=(path,options)=>path.startsWith('/api/runs?')?Promise.resolve(new Response(JSON.stringify({runs:[run('a','origin/team/api','refs/heads/feature/x','passed'),run('b','origin/team/api','refs/tags/v1.2','passed'),run('c',long,'refs/heads/'+'topic-'.repeat(12),'failed')]}))):original(path,options);
  try{await screen.read();
   return Array.from(document.querySelectorAll('.runs a.row')).map(row=>({repo:row.querySelector('.repo').textContent,repoTitle:row.querySelector('.repo').title,ref:row.querySelector('.ref').textContent,refTitle:row.querySelector('.ref').title,cut:['repo','ref'].map(name=>{const cell=row.querySelector('.'+name);return cell.scrollWidth>cell.clientWidth&&getComputedStyle(cell).textOverflow==='ellipsis';}),height:row.getBoundingClientRect().height,when:row.querySelector('.when').textContent}));}
  finally{screen.controller.abort();window.fetch=original;}
 })()`);
 check(result[0].repo==='api'&&result[0].repoTitle==='origin/team/api'&&result[0].ref==='feature/x'&&result[0].refTitle==='refs/heads/feature/x'&&result[1].ref==='tag v1.2'&&result[0].when==='15:04 · 4m 0s','short repository, ref or finish time wrong '+JSON.stringify(result));
 check(result[2].repoTitle.endsWith('name')&&result[2].cut[0]&&result[2].cut[1]&&!result[0].cut[0]&&result.every(row=>row.height===result[0].height),'long names were not cut with an ellipsis on one line '+JSON.stringify(result));
 return result;
}

async function lineNumbers(client){
 await client.loaded('/runs/22222222','.steps a.row');
 await settle(client,`Boolean(document.querySelector('.terminal .gutter')?.textContent)`,'log');
 const copied=await client.evaluate(`(()=>{
  const terminal=document.querySelector('.terminal'),pre=terminal.querySelector('pre'),range=document.createRange();
  range.selectNodeContents(terminal);const selection=getSelection();selection.removeAllRanges();selection.addRange(range);
  const result={selected:selection.toString(),log:pre.textContent,gutter:terminal.querySelector('.gutter').textContent};selection.removeAllRanges();return result;
 })()`);
 check(copied.gutter==='1\n2\n3\n4\n'&&copied.selected.trim()===copied.log.trim()&&!/^\s*1\s/.test(copied.selected),'line numbers were selected or copied with the log '+JSON.stringify(copied));
 // Five thousand lines: one text node of numbers beside the log text, timed with layout.
 const timing=await client.evaluate(`(()=>{
  const output=h('pre'),gutter=h('div',{class:'gutter','aria-hidden':'true'},''),box=h('div',{class:'terminal'},gutter,output);
  document.querySelector('.log').append(box);
  const lines=Array.from({length:5000},(_,index)=>'line '+(index+1)+' of a long build log with some text').join('\\n')+'\\n';
  const start=performance.now();resetLog({output,gutter,status:h('div')},lines);box.getBoundingClientRect();const reset=performance.now()-start;
  const regions={output,gutter,status:h('div')};regions.liveBytes=new TextEncoder().encode(output.textContent).length;regions.lines=5000;regions.open=false;
  const appendStart=performance.now();for(let chunk=0;chunk<50;chunk++)appendLive(regions,Array.from({length:100},(_,index)=>'appended '+(chunk*100+index)).join('\\n')+'\\n');box.getBoundingClientRect();const append=performance.now()-appendStart;
  const result={reset:Math.round(reset),append:Math.round(append),numbers:gutter.textContent.split('\\n').length-1,last:gutter.textContent.slice(-6),height:Math.abs(gutter.getBoundingClientRect().height-output.getBoundingClientRect().height)};
  box.remove();return result;
 })()`);
 check(timing.numbers===10000&&timing.last==='10000\n'&&timing.height<1&&timing.reset<1000&&timing.append<1000,'five thousand line log too slow or misnumbered '+JSON.stringify(timing));
 return {copied:copied.gutter,timing};
}

async function liveHiccup(client){
 await client.loaded('/runs','.runs a.row');await pause(20);
 const result=await client.evaluate(`(async()=>{
  const original=fetch,view=screen,plan=['fail','ok','fail','ok','fail','fail','ok'];let calls=0,shown=false;const states=new Set();
  const observer=new MutationObserver(()=>{if(notice.textContent)shown=true;states.add(server.dataset.state);});
  observer.observe(notice,{subtree:true,childList:true,characterData:true});observer.observe(server,{attributes:true});
  window.fetch=(path,options)=>{
   if(path!=='/mcp'||JSON.parse(options.body).params.name!=='events_wait')return original(path,options);
   const step=plan[calls++];
   if(step==='fail')return Promise.reject(Object.assign(new TypeError('connection cut'),{retry:25}));
   if(step==='ok')return Promise.resolve(new Response(JSON.stringify({result:{isError:false,structuredContent:{events:[],next_after_id:0}}})));
   return new Promise((_,reject)=>options.signal.addEventListener('abort',()=>reject(new DOMException('Aborted','AbortError')),{once:true}));
  };
  try{
   view.waiting.abort();
   for(let i=0;i<800&&calls<8;i++)await new Promise(resolve=>setTimeout(resolve,10));
   await new Promise(resolve=>setTimeout(resolve,50));
   return {calls,shown,states:[...states],final:notice.textContent};
  }finally{observer.disconnect();view.controller.abort();window.fetch=original;}
 })()`);
 // Each cut wait is followed by a read the server answers, so the connection never counts as lost.
 check(result.calls===8&&!result.shown&&result.states.every(state=>state==='normal')&&result.final==='','a cut live update that the next read recovers was not quiet '+JSON.stringify(result));
 return result;
}

export async function behavior(client){
 const result={burst:await burst(client),visibility:await hidden(client),observation:await idleAndRecovery(client)};
 result.importedIssue=await importedIssue(client);
 result.nativeIssues=await nativeIssues(client);
 result.issuePaging=await issueChipsAndPaging(client);
 result.issueSearch=await issueSearchAndStale(client);
 result.issueBack=await issueBackLink(client);
 result.offline=await offline(client);
 result.promote=await actions(client,'Promote','/runs/77777777','promote');
 result.release=await actions(client,'Release','/runs/33333333','run_hold_abandon');
 result.holds=await inspectHolds(client);

 result.confirmation=await confirmationAndFetch(client);
 result.refusedRerun=await refusedRerun(client);
 result.logs=await logAppend(client);
 result.pieces=await repositoryListAndChips(client);
 result.groups=await sideGroups(client);
 result.shortNames=await shortNameCollisions(client);
 result.runCells=await runCells(client);
 result.lineNumbers=await lineNumbers(client);
 result.liveHiccup=await liveHiccup(client);
 result.signout=await signout(client);
 client.checkFatal();
 return result;
}
