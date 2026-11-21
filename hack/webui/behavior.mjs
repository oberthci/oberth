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
   if(uiRecovery.waits===1)return new Response(JSON.stringify({reason:'Live unavailable.',remedy:'Retry the wait.'}),{status:503,headers:{'Retry-After':'1'}});
   if(uiRecovery.waits===2)return new Response(JSON.stringify({result:{isError:false,structuredContent:{events:[],next_after_id:18}}}));
   return new Promise((_,reject)=>options.signal.addEventListener('abort',()=>reject(new DOMException('Aborted','AbortError')),{once:true}));
  };
  view.waiting.abort();
 })()`);
 await pause(100);
 check((await client.evaluate(`notice.textContent`)).includes('Live updates stopped.'),'live failure was not shown');
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

async function offline(client){
 await client.loaded('/runs','.runs a.row');await pause(20);
 const result=await client.evaluate(`(async()=>{
  const original=fetch,view=screen;let maximum=0,reads=0,waits=0;
  const count=()=>{maximum=Math.max(maximum,notice.textContent.split(copy.network[0]).length-1);};
  const observer=new MutationObserver(count);observer.observe(notice,{subtree:true,childList:true});
  window.fetch=async path=>{if(path==='/mcp')waits++;else reads++;throw Object.assign(new TypeError('offline'),{retry:25});};
  try{view.lastRead=-Infinity;view.waiting.abort();await new Promise(resolve=>setTimeout(resolve,1150));count();return {maximum,reads,waits,notice:notice.textContent};}
  finally{view.controller.abort();window.fetch=original;observer.disconnect();}
 })()`);
 check(result.reads>=2&&result.waits>=2,'offline check missed the read/wait retry cycle '+JSON.stringify(result));
 check(result.maximum===1&&result.notice.split('Oberth is not reachable.').length===2,'offline notice appeared more than once');
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
 await deadline('load after sign-out',async()=>{const ready=client.nextEvent('Page.loadEventFired');await client.evaluate(`document.querySelector('#signout').click()`);await ready;});await pause(100);
 const result=await client.evaluate(`({hasToken:localStorage.getItem('oberth.token')!==null,form:!signin.hidden,mainHidden:main.hidden,notice:notice.textContent})`);
 check(!result.hasToken,'sign-out retained the bearer token');
 check(result.form&&result.mainHidden&&result.notice==='', 'first visit did not show a quiet sign-in form');
 await client.evaluate(`localStorage.setItem('oberth.token','invalid');route()`);
 await pause(100);
 check((await client.evaluate(`notice.textContent`)).includes('credentials'), 'invalid credentials did not show their refusal');
 await client.evaluate(`localStorage.setItem('oberth.token',${JSON.stringify(client.token)})`);
 return result;
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
  await client.loaded('/issues/'+id,'main h2');
  const result=await client.evaluate(`(()=>{
   const region=document.querySelector('main > .issues');
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

async function settle(client,test,label){
 for(let i=0;i<150;i++){if(await client.evaluate(test))return;await pause(20);}
 check(false,label+' did not settle');
}

async function repositoryListAndChips(client){
 await client.loaded('/runs','.runs a.row');await pause(20);
 const mark=client.requests.length;
 await client.evaluate(`Array.from(document.querySelectorAll('.repo-side a')).find(link=>link.title==='origin/team/web').click()`);
 await settle(client,`location.search==='?repo=origin%2Fteam%2Fweb'&&Boolean(document.querySelector('.runs a.row'))`,'repository choice');
 const side=await client.evaluate(`Array.from(document.querySelectorAll('.repo-side a')).map(link=>[link.title||link.textContent,link.getAttribute('aria-current')])`);
 check(JSON.stringify(side)===JSON.stringify([['All repos',null],['mirror/tools/cli',null],['origin/team/api',null],['origin/team/web','true']]),'chosen repository not marked '+JSON.stringify(side));
 check(client.requests.slice(mark).some(request=>request.path==='/api/runs'&&new URL(request.url).searchParams.get('repo')==='origin/team/web'),'repository list did not filter the run list');
 const chipMark=client.requests.length;
 await client.evaluate(`Array.from(document.querySelectorAll('.chips a')).find(link=>link.textContent==='failed').click()`);
 await settle(client,`new URLSearchParams(location.search).get('status')==='failed'&&Boolean(document.querySelector('.runs a.row'))`,'status chip');
 const chips=await client.evaluate(`({search:location.search,current:Array.from(document.querySelectorAll('.chips a[aria-current="true"]')).map(link=>link.textContent),side:document.querySelector('.repo-side a[aria-current="true"]').title})`);
 check(chips.search==='?repo=origin%2Fteam%2Fweb&status=failed'&&JSON.stringify(chips.current)==='["failed"]'&&chips.side==='origin/team/web','chip choice lost the address or the repository '+JSON.stringify(chips));
 check(client.requests.slice(chipMark).some(request=>{const query=new URL(request.url).searchParams;return request.path==='/api/runs'&&query.get('status')==='failed'&&query.get('repo')==='origin/team/web';}),'status chip did not filter the run list');
 await client.evaluate(`Array.from(document.querySelectorAll('.chips a')).find(link=>link.textContent==='all').click()`);
 await settle(client,`location.search==='?repo=origin%2Fteam%2Fweb'`,'all chip');
 return {side,chips};
}

const sideState=`({search:location.search,repos:Array.from(document.querySelectorAll('.repo-side a')).map(link=>link.title||link.textContent),groups:Array.from(document.querySelectorAll('.repo-group')).map(node=>node.textContent),runs:Array.from(document.querySelectorAll('.runs a.row .repo')).map(node=>node.textContent),boxes:Array.from(document.querySelectorAll('.upstreams label')).map(label=>[label.textContent,label.querySelector('input').checked,label.querySelector('input').disabled]),options:Array.from(document.querySelectorAll('.filters option')).map(option=>option.value)})`;
const clickUpstream=name=>`Array.from(document.querySelectorAll('.upstreams label')).find(label=>label.textContent===${JSON.stringify(name)}).querySelector('input').click()`;

async function upstreamSelector(client){
 await client.loaded('/runs','.runs a.row');await pause(20);
 const all=await client.evaluate(sideState);
 check(all.search===''&&all.runs.length===4&&JSON.stringify(all.boxes)===JSON.stringify([['mirror',true,false],['origin',true,false]])&&all.repos.length===4,'default did not select every upstream '+JSON.stringify(all));
 const mark=client.requests.length;
 await client.evaluate(clickUpstream('mirror'));
 await settle(client,`location.search==='?upstreams=origin'&&document.querySelectorAll('.runs a.row').length===3`,'upstream choice');
 const origin=await client.evaluate(sideState);
 check(JSON.stringify(origin.repos)===JSON.stringify(['All repos','origin/team/api','origin/team/web'])&&JSON.stringify(origin.groups)==='["origin/team"]'&&JSON.stringify(origin.options)===JSON.stringify(['','origin/team/api','origin/team/web']),'unchecked upstream kept its repositories '+JSON.stringify(origin));
 check(origin.runs.every(repo=>repo.startsWith('origin/'))&&JSON.stringify(origin.boxes)===JSON.stringify([['mirror',false,false],['origin',true,true]]),'unchecked upstream kept its runs, or the last box can be cleared '+JSON.stringify(origin));
 check(client.requests.slice(mark).some(request=>request.path==='/api/runs'&&new URL(request.url).searchParams.get('upstreams')==='origin'),'upstream choice was not filtered by the server');
 await client.evaluate(`history.back()`);
 await settle(client,`location.search===''&&document.querySelectorAll('.runs a.row').length===4&&document.querySelectorAll('.upstreams input:checked').length===2`,'back button');
 // A link reproduces the view; a chosen repository goes when its upstream is unchecked.
 await client.loaded('/runs?upstreams=mirror&repo=mirror%2Ftools%2Fcli','.runs a.row');
 const linked=await client.evaluate(sideState);
 check(JSON.stringify(linked.boxes)===JSON.stringify([['mirror',true,true],['origin',false,false]])&&JSON.stringify(linked.runs)==='["mirror/tools/cli"]','a link did not reproduce the upstream view '+JSON.stringify(linked));
 await client.evaluate(clickUpstream('origin'));
 await settle(client,`location.search==='?repo=mirror%2Ftools%2Fcli'&&document.querySelectorAll('.upstreams input:checked').length===2`,'second upstream');
 await client.evaluate(clickUpstream('mirror'));
 await settle(client,`location.search==='?upstreams=origin'`,'chosen repository dropped with its upstream');
 return {all,origin,linked};
}

async function shortNameCollisions(client){
 await client.loaded('/runs','.runs a.row');await pause(20);
 const listed=search=>client.evaluate(`(async()=>{
  const original=fetch,repos=['origin/team/web','origin/team/api','mirror/tools/cli','mirror/tools/api','mirror/team/web'];
  window.fetch=(path,options)=>path.startsWith('/api/repos?')?Promise.resolve(new Response(JSON.stringify({repositories:repos.map(repo=>({repo}))}))):original(path,options);
  try{history.replaceState(null,'',${JSON.stringify('/runs'+search)});await route();
   return {links:Array.from(document.querySelectorAll('.repo-side a')).map(link=>[link.textContent,link.getAttribute('aria-label')]),groups:Array.from(document.querySelectorAll('.repo-group')).map(node=>node.textContent),options:Array.from(document.querySelectorAll('.filters optgroup')).map(group=>[group.label,Array.from(group.children).map(option=>option.textContent)])};}
  finally{screen.controller.abort();window.fetch=original;}
 })()`);
 const all=await listed('');
 check(JSON.stringify(all.links)===JSON.stringify([['All repos',null],['mirror/team/web','mirror/team/web'],['tools/api','mirror/tools/api'],['cli','mirror/tools/cli'],['team/api','origin/team/api'],['origin/team/web','origin/team/web']])&&JSON.stringify(all.groups)===JSON.stringify(['mirror/team','mirror/tools','origin/team']),'colliding short names were not told apart '+JSON.stringify(all));
 check(JSON.stringify(all.options)===JSON.stringify([['mirror/team',['mirror/team/web']],['mirror/tools',['tools/api','cli']],['origin/team',['team/api','origin/team/web']]]),'repository select differs from the list '+JSON.stringify(all.options));
 const origin=await listed('?upstreams=origin');
 check(JSON.stringify(origin.links)===JSON.stringify([['All repos',null],['api','origin/team/api'],['web','origin/team/web']]),'hidden repositories still lengthened short names '+JSON.stringify(origin));
 return {all,origin};
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
  const original=fetch,view=screen,plan=['fail','ok','fail','ok','fail','fail','ok'];let calls=0,early=false,late=false;
  const observer=new MutationObserver(()=>{if(notice.textContent.includes(copy.live[0])){if(calls<=5)early=true;else late=true;}});
  observer.observe(notice,{subtree:true,childList:true,characterData:true});
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
   return {calls,early,late,final:notice.textContent};
  }finally{observer.disconnect();view.controller.abort();window.fetch=original;}
 })()`);
 check(result.calls===8&&!result.early&&result.late&&result.final==='','one cut live update was not quiet, or two in a row were not shown '+JSON.stringify(result));
 return result;
}

export async function behavior(client){
 const result={burst:await burst(client),visibility:await hidden(client),observation:await idleAndRecovery(client)};
 result.importedIssue=await importedIssue(client);
 result.nativeIssues=await nativeIssues(client);
 result.offline=await offline(client);
 result.promote=await actions(client,'Promote','/runs/77777777','promote');
 result.release=await actions(client,'Release','/runs/33333333','run_hold_abandon');
 result.holds=await inspectHolds(client);

 result.confirmation=await confirmationAndFetch(client);
 result.refusedRerun=await refusedRerun(client);
 result.logs=await logAppend(client);
 result.pieces=await repositoryListAndChips(client);
 result.upstreams=await upstreamSelector(client);
 result.shortNames=await shortNameCollisions(client);
 result.lineNumbers=await lineNumbers(client);
 result.liveHiccup=await liveHiccup(client);
 result.signout=await signout(client);
 client.checkFatal();
 return result;
}
