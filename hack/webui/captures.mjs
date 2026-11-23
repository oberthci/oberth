import {writeFile} from 'node:fs/promises';
import {check} from './cdp.mjs';

export async function captures(client){
 const {cdp,evaluate,loaded,events,origin}=client,shots=client.artifacts;
const results=[];
 let runRowHeight;
 for(const width of [360,1280]){
  await cdp('Emulation.setDeviceMetricsOverride',{width,height:900,deviceScaleFactor:1,mobile:false});
  for(const theme of ['light','dark','mono','color']){
   await cdp('Emulation.setEmulatedMedia',{features:[{name:'prefers-color-scheme',value:theme==='light'?'light':'dark'},{name:'prefers-reduced-motion',value:'reduce'}]});
   for(const [page,path,selector] of [['runs','/runs','.runs a.row'],['run','/runs/22222222','.steps a.row'],['issues','/issues','.issues a.row'],['issue','/issues/42','.issue h1']]){
    await loaded(path,selector);
    await evaluate(`document.getAnimations().forEach(animation=>animation.cancel());document.documentElement.dataset.theme=${JSON.stringify(theme)};`);
    const metrics=await evaluate(`({width:innerWidth,scroll:document.documentElement.scrollWidth,errors:document.querySelector('#notice').textContent,rows:document.querySelectorAll(${JSON.stringify(selector)}).length,secret:document.querySelectorAll('.secret').length,steps:Array.from(document.querySelectorAll('.steps .row')).map(node=>node.dataset.key),reason:Array.from(document.querySelectorAll('.message')).find(node=>node.textContent.length===40)?(()=>{const node=Array.from(document.querySelectorAll('.message')).find(node=>node.textContent.length===40);const range=document.createRange();range.selectNodeContents(node);return {length:node.textContent.length,height:range.getBoundingClientRect().height,lineHeight:getComputedStyle(node).lineHeight,width:range.getBoundingClientRect().width}})():null})`);
    check(metrics.width===width && metrics.scroll<=width,'horizontal overflow '+JSON.stringify(metrics));
    check(!metrics.errors,'page error '+metrics.errors);
    if(page==='run'){check(metrics.secret===1,'missing secret tag');check(metrics.steps.join(',')==='z-checkout,y-build,x-test,w-publish','plan order changed');}
    // One line per step, every row as high as a run row (37px); a long name is cut with an
    // ellipsis and keeps its whole text as the title; the chosen step keeps its accent bar.
    if(page==='run'){
     metrics.stepRows=await evaluate(`Array.from(document.querySelectorAll('.steps .row')).map(row=>{const name=row.querySelector('.step-name'),style=getComputedStyle(name),bottom=node=>{const range=document.createRange();range.selectNodeContents(node);return range.getBoundingClientRect().bottom;};return {level:[row.querySelector('.state').lastChild,name.firstChild,row.querySelector('.duration').firstChild].filter(node=>node&&node.textContent.trim()).map(bottom),height:row.getBoundingClientRect().height,text:name.textContent,title:name.title,cut:name.scrollWidth>name.clientWidth&&style.textOverflow==='ellipsis',current:row.getAttribute('aria-current'),bar:getComputedStyle(row).borderLeftColor!=='rgba(0, 0, 0, 0)'};})`);
     const long=metrics.stepRows.find(row=>row.text==='install-release-support-and-golangci-lint');
     check(metrics.stepRows.every(row=>Math.abs(row.height-37)<0.5)&&long&&long.cut&&long.title===long.text&&metrics.stepRows.filter(row=>row.cut).length===1&&metrics.stepRows.every(row=>Math.max(...row.level)-Math.min(...row.level)<1),'step rows not one 37px line each, or the long name not cut with its title '+JSON.stringify({width,theme,rows:metrics.stepRows}));
     check(metrics.stepRows.filter(row=>row.current==='step').length===1&&metrics.stepRows.find(row=>row.current==='step').text==='test'&&metrics.stepRows.every(row=>row.bar===(row.current==='step')),'the failed step is not the chosen one '+JSON.stringify(metrics.stepRows));
    }
    if(page==='runs'&&width===360){check(metrics.reason&&metrics.reason.height<=parseFloat(metrics.reason.lineHeight),'40-character reason wrapped '+JSON.stringify(metrics));}
    metrics.rowHeights=await evaluate(`Array.from(document.querySelectorAll('.runs a.row')).map(row=>{const style=getComputedStyle(row),height=row.getBoundingClientRect().height;const lineHeight=parseFloat(style.lineHeight);return {height,lineHeight,content:height-parseFloat(style.paddingTop)-parseFloat(style.paddingBottom)-parseFloat(style.borderBottomWidth)-parseFloat(style.borderTopWidth),when:Array.from(row.querySelector('.when').children).map(node=>node.textContent).join('|'),title:row.querySelector('.when').title,message:Boolean(row.querySelector('.message').textContent),cells:Array.from(row.children).filter(cell=>!cell.matches('.state')).map(cell=>{const walker=document.createTreeWalker(cell,NodeFilter.SHOW_TEXT,{acceptNode:node=>node.textContent.trim()&&node.parentElement.checkVisibility()?NodeFilter.FILTER_ACCEPT:NodeFilter.FILTER_SKIP}),text=walker.nextNode(),range=document.createRange();if(text)range.selectNodeContents(text);const box=text?range.getBoundingClientRect():{bottom:0,height:0},all=document.createRange();all.selectNodeContents(cell);return {name:cell.className,size:getComputedStyle(cell).fontSize,bottom:box.bottom,height:all.getBoundingClientRect().height,cut:cell.scrollWidth>cell.clientWidth+0.5};})};})`);
    // One line per run on a wide screen, every cell at one size on one baseline and nothing cut in the
    // example; a phone adds a second line only for a message.
    if(page==='runs')for(const row of metrics.rowHeights){
     const lines=width===1280||!row.message?1:2;
     check(row.content<=lines*row.lineHeight+0.5&&(width!==1280||Math.abs(row.height-metrics.rowHeights[0].height)<0.5),'run row is not '+lines+' line(s) or rows not uniform '+JSON.stringify({width,...row}));
     const first=row.cells.filter(cell=>cell.height>0&&(width===1280||!cell.name.includes('message')));
     check(row.cells.every(cell=>cell.size===row.cells[0].size&&cell.height<=row.lineHeight+0.5)&&first.every(cell=>Math.abs(cell.bottom-first[0].bottom)<1),'run cells differ in size or baseline '+JSON.stringify({width,...row}));
     if(width===1280)check(row.cells.every(cell=>!cell.cut),'example run cell cut at 1280 '+JSON.stringify(row.cells));
     check(!/AM|PM|0m \d+s|\d{3}m/.test(row.when),'long time form '+row.when);
    }
    if(page==='runs')check(JSON.stringify(metrics.rowHeights.map(row=>row.when))===JSON.stringify(['15:00|48s','15:00|48s','started |15:00|6h 22m','queued |15:00']),'run-list finish, start or queue time changed '+JSON.stringify(metrics.rowHeights.map(row=>row.when)));
    if(page==='runs'){
     metrics.columns=await evaluate(`({heading:getComputedStyle(document.querySelector('.runs .head')).display,positions:Array.from(document.querySelectorAll('.runs a.row')).map(row=>['state','repo','ref','when','message'].map(name=>row.querySelector('.'+name).getBoundingClientRect().left))})`);
     if(width===360)check(metrics.columns.heading==='none','narrow heading visible');
     else check(metrics.columns.positions.every(row=>row.every((left,index)=>Math.abs(left-metrics.columns.positions[0][index])<0.5)),'run columns are misaligned');
     // The repository list sits left of the table on wide screens; a phone keeps the select. Status chips replace the status select.
     metrics.pieces=await evaluate(`({side:getComputedStyle(document.querySelector('.repo-side')).display,sideLinks:Array.from(document.querySelectorAll('.repo-side a')).map(link=>[link.textContent,link.title||null,link.getAttribute('aria-current')]),groups:Array.from(document.querySelectorAll('.repo-side button.repo-group')).map(node=>[node.textContent,node.getAttribute('aria-expanded')]),upstreams:document.querySelectorAll('.upstreams, main input[type=checkbox]').length,heading:document.querySelectorAll('.side-head').length,options:Array.from(document.querySelector('.filters select').children).map(node=>node.label||node.textContent).concat(Array.from(document.querySelectorAll('.filters optgroup option')).map(node=>node.textContent)),select:getComputedStyle(document.querySelector('.filters > label')).display,chips:Array.from(document.querySelectorAll('.chips a')).map(link=>[link.textContent,link.getAttribute('aria-current')]),selects:document.querySelectorAll('main select').length,foot:document.querySelector('.runs-foot').textContent,sideTop:document.querySelector('.repo-side').getBoundingClientRect().top,tableTop:document.querySelector('.runs').getBoundingClientRect().top,sideRows:Array.from(document.querySelector('.repo-side').querySelectorAll('.repo-group,.repo-filter')).map(node=>node.getBoundingClientRect().height),rowHeight:document.querySelector('.runs a.row').getBoundingClientRect().height,tableHead:document.querySelector('.runs .head').getBoundingClientRect().height})`);
     check(JSON.stringify(metrics.pieces.sideLinks)===JSON.stringify([['All repos',null,'true'],['cli','mirror/tools/cli',null],['api','origin/team/api',null],['web','origin/team/web',null]])&&JSON.stringify(metrics.pieces.groups)===JSON.stringify([['mirror/tools','true'],['origin/team','true']])&&metrics.pieces.upstreams===0&&metrics.pieces.heading===0&&JSON.stringify(metrics.pieces.options)===JSON.stringify(['all repositories','mirror/tools','origin/team','cli','api','web'])&&JSON.stringify(metrics.pieces.chips)===JSON.stringify([['all','true'],['queued',null],['running',null],['passed',null],['failed',null],['cancelled',null]])&&metrics.pieces.selects===1&&metrics.pieces.foot==='Newest first · 50 runs per page','repository list, groups, chips or footer changed '+JSON.stringify(metrics.pieces));
     // Every side row (All repos, each group, each repository) is one table row high, and the first lines up with the table head.
     if(width===1280)check(metrics.pieces.side!=='none'&&metrics.pieces.select==='none'&&Math.abs(metrics.pieces.sideTop-metrics.pieces.tableTop)<0.5&&metrics.pieces.sideRows.every(height=>Math.abs(height-metrics.pieces.rowHeight)<=1)&&Math.abs(metrics.pieces.sideRows[0]-metrics.pieces.tableHead)<=1,'wide screen lost the repository list, or the list and the table lost one rhythm '+JSON.stringify(metrics.pieces));
     else check(metrics.pieces.side==='none'&&metrics.pieces.select!=='none','phone lost the repository select '+JSON.stringify(metrics.pieces));
    }
    if(page==='run'){
     metrics.runPieces=await evaluate(`(async()=>{for(let i=0;i<100&&!document.querySelector('.terminal .gutter')?.textContent;i++)await new Promise(resolve=>setTimeout(resolve,20));const gutter=document.querySelector('.terminal .gutter'),pre=document.querySelector('.terminal > pre');return {head:document.querySelector('.steps-head').textContent,failed:document.querySelector('.run-head .failed-step')?.textContent,container:document.querySelector('.log .toolbar label')?.firstChild.textContent,select:Boolean(document.querySelector('.log .toolbar label select')),gutter:gutter.textContent,gutterLine:getComputedStyle(gutter).lineHeight,preLine:getComputedStyle(pre).lineHeight,gutterTop:gutter.getBoundingClientRect().top,preTop:pre.getBoundingClientRect().top,selectable:getComputedStyle(gutter).userSelect};})()`);
     check(metrics.runPieces.head==='Steps · 4'&&metrics.runPieces.failed==='test'&&metrics.runPieces.container==='Container'&&metrics.runPieces.select,'run page pieces missing '+JSON.stringify(metrics.runPieces));
     check(metrics.runPieces.gutter==='1\n2\n3\n4\n'&&metrics.runPieces.gutterLine===metrics.runPieces.preLine&&Math.abs(metrics.runPieces.gutterTop-metrics.runPieces.preTop)<0.5&&metrics.runPieces.selectable==='none','line numbers misaligned or selectable '+JSON.stringify(metrics.runPieces));
    }
    // The issue workspace: seven counted chips, search, a pager above and below, and the run list's rows: one
    // line of the run row's height on a wide screen, at most two on a phone. Only the long title of #65 is
    // cut, and it keeps its whole text as its title.
    if(page==='runs')runRowHeight=metrics.rowHeights[0].height;
    if(page==='issues'){
     metrics.issues=await evaluate(`({chips:document.querySelectorAll('.chips a .count').length,pages:Array.from(document.querySelectorAll('.pages .position')).map(node=>node.textContent),search:Boolean(document.querySelector('input.issue-search')),rows:Array.from(document.querySelectorAll('.issues a.row')).map(row=>{const style=getComputedStyle(row),title=row.querySelector('.title');return {key:row.dataset.key,height:row.getBoundingClientRect().height,lines:(row.getBoundingClientRect().height-parseFloat(style.paddingTop)-parseFloat(style.paddingBottom)-parseFloat(style.borderBottomWidth))/parseFloat(style.lineHeight),cut:title.scrollWidth>title.clientWidth,whole:title.title===title.textContent};})})`);
     check(metrics.issues.chips===7&&JSON.stringify(metrics.issues.pages)==='["1-50 of 64","1-50 of 64"]'&&metrics.issues.search,'issue workspace pieces missing '+JSON.stringify(metrics.issues));
     const wrong=metrics.issues.rows.filter(row=>!row.whole||(width===1280?Math.abs(row.height-runRowHeight)>=0.5||row.lines>1.02:row.lines>2.02));
     check(!wrong.length,'issue rows are not the run list rows '+JSON.stringify({width,runRowHeight,wrong}));
     if(width===1280)check(JSON.stringify(metrics.issues.rows.filter(row=>row.cut).map(row=>row.key))==='["65"]','only the long title is cut '+JSON.stringify(metrics.issues.rows.filter(row=>row.cut)));
    }
    if(page==='issue'){
     metrics.issue=await evaluate(`({back:document.querySelector('main > a.back')?.getAttribute('href'),states:Array.from(document.querySelectorAll('.issue-states .state')).map(node=>node.textContent),lock:document.querySelector('.issue-states .lock')?.textContent})`);
     check(metrics.issue.back==='/issues'&&JSON.stringify(metrics.issue.states)==='["open","active"]'&&metrics.issue.lock==='editor · 5m left','issue page pieces missing '+JSON.stringify(metrics.issue));
    }
    metrics.fonts=await evaluate(`({body:getComputedStyle(document.body).fontFamily,log:document.querySelector('.terminal > pre')?getComputedStyle(document.querySelector('.terminal > pre')).fontFamily:null})`);
    if(theme==='light'||theme==='dark'){
     // The default design is monospace throughout: every element that carries text, and every control.
     metrics.fonts.text=await evaluate(`Array.from(new Set(Array.from(document.querySelectorAll('body *')).filter(node=>node.matches('button,select,input,option')||Array.from(node.childNodes).some(child=>child.nodeType===3&&child.textContent.trim())).map(node=>getComputedStyle(node).fontFamily)))`);
     check(metrics.fonts.body.includes('monospace')&&!metrics.fonts.body.includes('system-ui')&&metrics.fonts.text.length&&metrics.fonts.text.every(family=>family.includes('monospace')&&!/system-ui|sans-serif/.test(family)),'default design is not monospace throughout '+JSON.stringify(metrics.fonts));
    } else check(metrics.fonts.body.includes('system-ui')&&(page!=='run'||Boolean(metrics.fonts.log?.includes('monospace'))),'font scope is wrong');
    metrics.textCharacters=await evaluate(`Array.from(new Set(Array.from(document.body.textContent).filter(character=>character.codePointAt(0)>127&&character!=='·')))`);
    check(!metrics.textCharacters.length,'unlisted page character '+JSON.stringify(metrics.textCharacters));
    metrics.marks=await evaluate(`Array.from(document.querySelectorAll('a.row .mark')).map(mark=>{const style=getComputedStyle(mark),box=mark.getBoundingClientRect();return {text:mark.textContent,width:box.width,height:box.height,radius:style.borderRadius,border:style.borderTopStyle};})`);
    check(metrics.marks.length===({runs:4,run:4,issues:50,issue:0})[page]&&metrics.marks.every(mark=>!mark.text&&mark.width===7&&mark.height===7&&mark.radius==='50%'&&mark.border==='solid'),'CSS marks missing');
    if(page==='run'){
     metrics.identifiers=await evaluate(`({text:document.querySelector('.identity').textContent,started:document.querySelector('.run-meta time').textContent,run:document.querySelector('.identity [title="Run ID"]').textContent,commit:document.querySelector('.identity [title="Commit ID"]').textContent,height:document.querySelector('.identity').getBoundingClientRect().height,lineHeight:parseFloat(getComputedStyle(document.querySelector('.identity')).lineHeight),detail:Array.from(document.querySelectorAll('main>section details pre')).map(node=>node.textContent).join('\\n'),right:document.querySelector('.identity').lastElementChild.getBoundingClientRect().right,container:document.querySelector('.identity').getBoundingClientRect().right})`);
     check(metrics.identifiers.run==='run 22222222'&&metrics.identifiers.commit==='commit 1234567890ab'&&metrics.identifiers.text==='run 22222222 · commit 1234567890ab'&&metrics.identifiers.started==='15:00'&&metrics.identifiers.height<=metrics.identifiers.lineHeight+0.5&&metrics.identifiers.right<=metrics.identifiers.container+0.5,'identifier header wrapped or wrong '+JSON.stringify(metrics.identifiers));
     check(metrics.identifiers.detail.includes('Run ID: 22222222222222222222222222222222')&&metrics.identifiers.detail.includes('Object ID: 1234567890abcdef1234567890abcdef12345678')&&metrics.identifiers.detail.includes('Commit ID: 1234567890abcdef1234567890abcdef12345678')&&metrics.identifiers.detail.includes('GMT+0100'),'full identifiers/timestamp missing from Detail');
     metrics.origin=await evaluate(`(()=>{const node=document.querySelector('main>section [aria-label="Started by"]');return node&&{text:node.textContent,elements:node.childElementCount,height:node.getBoundingClientRect().height,lineHeight:parseFloat(getComputedStyle(node).lineHeight)};})()`);
     check(metrics.origin&&metrics.origin.text===' · branch · by worker · assist requested'&&metrics.origin.elements===0&&metrics.origin.height<=metrics.origin.lineHeight+0.5,'run origin line missing, wrapped or not plain text '+JSON.stringify(metrics.origin));
    }
    const clip=await evaluate(`({x:0,y:0,width:innerWidth,height:Math.max(innerHeight,document.documentElement.scrollHeight),scale:1})`);
    const capture=await cdp('Page.captureScreenshot',{format:'png',captureBeyondViewport:true,clip});
    await writeFile(`${shots}/${page}-${width}-${theme}.png`,Buffer.from(capture.data,'base64'));
    results.push({page,width,theme,...metrics});
   }
  }
 }
 await cdp('Emulation.setDeviceMetricsOverride',{width:1280,height:900,deviceScaleFactor:1,mobile:false});
 await cdp('Emulation.setEmulatedMedia',{features:[{name:'prefers-color-scheme',value:'light'},{name:'prefers-reduced-motion',value:'reduce'}]});
 for(const [page,path,selector]of [['repositories','/repos','.repos a.row'],['issues','/issues','.issues a.row'],['issue','/issues/42','.issue h1'],['status','/status','main section p']]){
  await loaded(path,selector);await evaluate(`document.documentElement.dataset.theme='light';document.getAnimations().forEach(animation=>animation.cancel());`);
  const clip=await evaluate(`({x:0,y:0,width:innerWidth,height:Math.max(innerHeight,document.documentElement.scrollHeight),scale:1})`);
    const capture=await cdp('Page.captureScreenshot',{format:'png',captureBeyondViewport:true,clip});
  await writeFile(`${shots}/${page}-1280-light.png`,Buffer.from(capture.data,'base64'));
  const metrics=await evaluate(`({width:innerWidth,scroll:document.documentElement.scrollWidth,text:document.querySelector('main').textContent,errors:document.querySelector('#notice').textContent})`);
  check(metrics.scroll<=1280&&!metrics.errors,'extra screen overflow or error '+page);results.push({page,theme:'light',...metrics});
  metrics.current=await evaluate(`Array.from(document.querySelectorAll('nav a[aria-current="page"]')).map(link=>link.pathname)`);
  check(JSON.stringify(metrics.current)===JSON.stringify(['/'+path.split('/')[1]]),'current tab not marked '+JSON.stringify(metrics.current));
  if(page==='status'){metrics.cards=await evaluate(`Array.from(document.querySelectorAll('main .card')).map(card=>card.textContent)`);check(JSON.stringify(metrics.cards)===JSON.stringify(['Serverreadyversion 1','Runs queued1','Runs running2','Runs passed140','Runs failed9','Runs cancelled3']),'status cards changed '+JSON.stringify(metrics.cards));}
 }
 const formats=await evaluate(`({durations:[0,48,59,60,372,3599,3600,22920].map(seconds=>duration('1970-01-01T00:00:00Z',new Date(seconds*1000).toISOString())),today:time('2026-11-16T14:00:00Z'),older:time('2026-11-15T14:00:00Z'),fullTimestamp:fullTime('2026-11-16T14:00:00.123456789Z')})`);
 check(JSON.stringify(formats.durations)===JSON.stringify(['0s','48s','59s','1m 0s','6m 12s','59m 59s','1h 0m','6h 22m'])&&formats.today==='15:00'&&formats.older==='Nov 15 15:00'&&formats.fullTimestamp.includes('2026-11-16T14:00:00.123456789Z')&&formats.fullTimestamp.includes('GMT+0100'),'time formats failed '+JSON.stringify(formats));
 await cdp('Emulation.setDeviceMetricsOverride',{width:760,height:900,deviceScaleFactor:1,mobile:false});
 await loaded('/runs','.runs a.row');
 const intermediate=await evaluate(`({width:innerWidth,scroll:document.documentElement.scrollWidth,columns:getComputedStyle(document.querySelector('.runs')).gridTemplateColumns.split(' ').length,heights:Array.from(document.querySelectorAll('.runs a.row')).map(row=>row.getBoundingClientRect().height),reason:Array.from(document.querySelectorAll('.runs a.row .message')).every(node=>node.textContent.length>40||node.scrollWidth<=node.clientWidth)})`);
 check(intermediate.columns===5&&intermediate.scroll<=760&&intermediate.heights.every(height=>height===intermediate.heights[0])&&intermediate.reason,'tablet layout lost five one-line columns or cut a reason '+JSON.stringify(intermediate));
 await loaded('/runs/22222222','.steps a.row');
 const rerun=await evaluate(`(async()=>{
  const original=window.fetch,calls=[];
  window.fetch=(path,options)=>{if(path==='/mcp'&&JSON.parse(options.body).params.name==='run_rerun')calls.push(JSON.parse(options.body).params);return original(path,options);};
  const changed=new Promise(resolve=>{const observer=new MutationObserver(()=>{if(document.querySelector('.identity [title="Run ID"]')?.textContent==='run 55555555'){observer.disconnect();resolve();}});observer.observe(document.querySelector('main'),{childList:true,subtree:true});});
  try{Array.from(document.querySelectorAll('main button')).find(node=>node.textContent==='Rerun').click();await changed;return {path:location.pathname,calls};}finally{window.fetch=original;}
 })()`);
 check(rerun.path==='/runs/55555555'&&rerun.calls.length===1,'rerun did not navigate once '+JSON.stringify(rerun));
 await loaded('/runs/77777777','.steps a.row');
 const promotion=await evaluate(`(async()=>{
  const original=window.fetch,calls=[];
  window.fetch=(path,options)=>{if(path==='/mcp'&&JSON.parse(options.body).params.name==='promote')calls.push(JSON.parse(options.body).params);return original(path,options);};
  const changed=new Promise(resolve=>{const observer=new MutationObserver(()=>{if(document.querySelector('#notice').textContent.includes('Request accepted.')){observer.disconnect();resolve();}});observer.observe(document.querySelector('#notice'),{childList:true,subtree:true});});
  try{Array.from(document.querySelectorAll('main button')).find(node=>node.textContent==='Promote').click();await changed;return calls;}finally{window.fetch=original;}
 })()`);
 check(promotion.length===1&&promotion[0].arguments.sha==='1234567890abcdef1234567890abcdef12345678','promotion did not use the tested commit');
 await loaded('/repos','.repos a.row');check((await evaluate(`document.querySelector('.repos').textContent`)).includes('origin/team/api'),'repo absent');
 await loaded('/issues','.issues a.row');
 await loaded('/status','main section p');
 await loaded('/runs/33333333','.steps a.row');
 check((await evaluate(`document.querySelector('main').textContent`)).includes('Release'),'hold action absent');
 await loaded('/runs','.runs a.row');
 const liveCheck = await evaluate(`(async()=>{
  const original=window.fetch, view=screen, originalRead=view.read;
  let pending=[], outstanding=0, maximum=0, reads=0, ready, changed;
  const waiting=new Promise(resolve=>ready=resolve), refreshed=new Promise(resolve=>changed=resolve);
  view.read=async()=>{reads++;await originalRead();changed();};
  window.fetch=(path,options)=>{
   if(path!=='/mcp'||JSON.parse(options.body).params.name!=='events_wait')return original(path,options);
   outstanding++;maximum=Math.max(maximum,outstanding);
   return new Promise((resolve,reject)=>{
    const abort=()=>{outstanding--;reject(new DOMException('Aborted','AbortError'));};
    options.signal.addEventListener('abort',abort,{once:true});
    pending.push(()=>{options.signal.removeEventListener('abort',abort);outstanding--;resolve(new Response(JSON.stringify({result:{isError:false,structuredContent:{events:[{id:1}],next_after_id:1}}}),{headers:{'Content-Type':'application/json'}}));});
    ready();
   });
  };
  try{
   const rows=document.querySelectorAll('.runs a.row');rows[1].focus();
   view.waiting.abort();await waiting;
   pending.shift()();await refreshed;
   const focused=document.activeElement.dataset.key;
   document.activeElement.dispatchEvent(new KeyboardEvent('keydown',{key:'j',bubbles:true}));
   const keyboard=document.activeElement.dataset.key;
   return {maximum,reads,focused,keyboard,cursor:view.cursor};
  }finally{view.controller.abort();window.fetch=original;}
 })()`);
 check(liveCheck.maximum===1&&liveCheck.reads===1&&liveCheck.focused==='22222222222222222222222222222222'&&liveCheck.keyboard==='33333333333333333333333333333333'&&liveCheck.cursor===1,'live refresh or keyboard focus failed '+JSON.stringify(liveCheck));
 await loaded('/runs/33333333','.steps a.row');
 const interaction = await evaluate(`(async()=>{
  const original = window.fetch;
  let calls = [];
  window.fetch = async(path, options) => {
   if (path === "/mcp" && options?.body) {
    const request = JSON.parse(options.body);
    if (request.params.name !== "events_wait") calls.push(request.params);
   }
   return original(path,options);
  };
  let wait;
  const changed = new Promise(resolve=>wait=resolve);
  const observer = new MutationObserver(()=>{if(document.querySelector('#notice').textContent.includes('This run cannot be cancelled.')){observer.disconnect();wait();}});
  observer.observe(document.querySelector('#notice'),{childList:true,subtree:true});
  window.confirm = () => true;
  Array.from(document.querySelectorAll('main button')).find(node=>node.textContent==='Cancel').click();
  await changed;
  const refusal = document.querySelector('#notice').textContent;
  window.fetch = original;
  return {calls,refusal};
 })()`);
 check(interaction.calls.length===1 && interaction.calls[0].name==='run_cancel' && interaction.calls[0].arguments.reason==='web UI','action replayed or arguments changed');
 check(interaction.refusal.includes('Read the current run.')&&interaction.refusal.includes('complete server detail'),'server refusal changed');
 const streamBounds = await evaluate(`(async()=>{
  const original = window.fetch, results = [];
  try {
   for (const bytes of [1048576,1048577]) {
    window.fetch = async()=>new Response('x'.repeat(bytes));
    const regions = {output:h('pre'),status:h('div')};
    await streamLog('/api/runs/12345678',{node_id:'node',container:'main'},{regions,signal:new AbortController().signal});
    results.push({input:bytes,output:new TextEncoder().encode(regions.output.textContent).length,notice:regions.status.textContent});
   }
   return results;
  } finally {window.fetch=original;}
 })()`);
 check(streamBounds.every(item=>item.output===1048576)&&!streamBounds[0].notice&&streamBounds[1].notice,'stream byte boundary failed');
 const logEdges = await evaluate(`(async()=>{
  const regions={output:h('pre'),status:h('div'),controls:h('div')};
  appendLive(regions,'😀'.repeat(262144)+'ab');
  const unicode={bytes:new TextEncoder().encode(regions.output.textContent).length,replacement:regions.output.textContent.includes('�')};
  const original=window.fetch,view={logs:new AbortController()};
  let reads=0,release,ready;
  const done=new Promise(resolve=>ready=resolve);
  const observer=new MutationObserver(()=>{if(regions.output.textContent==='fresh'){observer.disconnect();ready();}});
  observer.observe(regions.output,{childList:true,subtree:true});
  window.fetch=async()=>{
   reads++;
   if(reads===1)return new Promise(resolve=>release=()=>resolve(new Response(JSON.stringify({content:'first',meta:{follow:false}}))));
   return new Response(JSON.stringify({content:'fresh',containers:[{node_id:'failed',container:'init-artifact-oberth'},{node_id:'failed',container:'main'}],meta:{follow:false}}));
  };
  try{
   const pending=loadLog(view,'/api/runs/12345678',{step:'build'},regions,false);
   view.logDirty=true;release();await pending;await done;
   const copied={output:h('pre'),status:h('div'),controls:h('div')};
   let copyReads=0;
   const copyPages=[{content:'earlier',meta:{follow:true}},{content:'earlier',meta:{copying:true,follow:false}},{content:'copied',meta:{copying:false,follow:false}}];
   window.fetch=async(input,init)=>String(input.url||input).includes('/api/runs/12345678/logs?')?new Response(JSON.stringify(copyPages[copyReads++])):original(input,init);
   await loadLog({logs:new AbortController()},'/api/runs/12345678',{step:'build'},copied,false);
   return {unicode,reads,content:regions.output.textContent,source:regions.status.textContent,copyReads,copyContent:copied.output.textContent};
  }finally{view.logs.abort();window.fetch=original;}
 })()`);
 check(logEdges.unicode.bytes<=1048576&&!logEdges.unicode.replacement&&logEdges.reads===2&&logEdges.content==='fresh'&&logEdges.source.includes('Container: init-artifact-oberth')&&logEdges.source.includes('Container: main')&&logEdges.copyReads===3&&logEdges.copyContent==='copied','live log edge check failed '+JSON.stringify(logEdges));
 
 await evaluate(`localStorage.setItem('oberth.token','invalid')`);
 await loaded('/runs','#signin:not([hidden])');
 const auth = await evaluate(`({reason:document.querySelector('#notice').textContent,password:document.querySelector('#signin input').type,hidden:document.querySelector('main').hidden})`);
 check(auth.reason.includes('Caller credentials are invalid.')&&auth.reason.includes('Reconnect with a current bearer')&&auth.hidden&&auth.password==='password','401 form or refusal lost '+JSON.stringify(auth));
 const errors=events.filter(event=>event.method==='Runtime.exceptionThrown');
 check(!errors.length,'script exceptions '+JSON.stringify(errors));
 const origins=events.filter(event=>event.method==='Network.requestWillBeSent').map(event=>event.params.request.url).filter(url=>!url.startsWith(origin+'/')&&!url.startsWith('data:'));
 check(!origins.length,'external page request');
 await writeFile(`${shots}/browser-checks.json`,JSON.stringify({results,scriptErrors:errors.length,externalRequests:origins,interaction,streamBounds,auth,liveCheck,logEdges,intermediate,rerun,promotion,formats},null,2));
 return {results,formats};

}
