import {launch,check,deadline} from './cdp.mjs';

export async function panelState() {
 const path='/runs/'+'c'.repeat(32);
 const client=await launch({env:{OBERTH_UI_PROGRESS:'1'},artifacts:process.env.OBERTH_UI_ARTIFACTS+'/panel-state'});
 try {
  await client.cdp('Page.addScriptToEvaluateOnNewDocument',{source:`
   const panelFetch=fetch;window.panelReads={logs:0,phase:''};
   window.fetch=async(path,options)=>{
    const url=new URL(path,location.href),response=await panelFetch(path,options);
    if(url.pathname.endsWith('/logs'))panelReads.logs++;
    if(url.pathname===${JSON.stringify('/api'+path)})panelReads.phase=(await response.clone().json()).steps.find(step=>step.plan_path==='test').detail;
    return response;
   };`});
  await client.loaded(path,'.log h2');
  const advance=async phase=>{
   await deadline('commit panel '+phase,()=>client.evaluate(`fetch('/example/progress?phase=${phase}',{method:'POST'}).then(async response=>{if(!response.ok)throw new Error(await response.text());})`));
   await deadline('observe panel '+phase,()=>client.evaluate(`new Promise(resolve=>{const test=()=>{if(panelReads.phase===${JSON.stringify(phase)}&&!screen.reading&&!screen.logBusy)resolve();else setTimeout(test,10);};test();})`));
  };
  await advance('steps');
  await client.evaluate(`(()=>{
   screen.logHeader.querySelector('summary').click();
   const range=document.createRange();range.selectNodeContents(document.querySelector('.terminal pre'));
   getSelection().removeAllRanges();getSelection().addRange(range);
   window.panelSelection=getSelection().toString();
  })()`);
  const inspect=()=>client.evaluate(`(()=>{
   const more=screen.logHeader.querySelector('details'),selection=getSelection(),range=selection.rangeCount?selection.getRangeAt(0):null;
   const rect=range?.getBoundingClientRect();
   return {open:more.open,detail:more.querySelector('pre').textContent,detailHeight:more.querySelector('pre').getBoundingClientRect().height,selection:selection.toString(),expected:panelSelection,selectionWidth:rect?.width||0,selectionHeight:rect?.height||0,logs:panelReads.logs};
  })()`);
  const preserved=result=>check(result.open&&result.detailHeight>0&&result.selection===result.expected&&result.selection.length>0&&result.selectionWidth>0&&result.selectionHeight>0,'live update lost open Detail or visible log selection '+JSON.stringify(result));
  const before=await inspect();preserved(before);
  check(before.logs===1,'finished step was not fetched once '+JSON.stringify(before));
  for(const phase of ['other-1','other-2','other-3'])await advance(phase);
  const other=await inspect();preserved(other);
  check(other.logs===1,'other steps re-read a finished log '+JSON.stringify(other));
  await advance('detail');
  const changed=await inspect();preserved(changed);
  check(changed.detail==='Updated prepare diagnostics.'&&changed.logs===2,'selected step did not refresh its own changed projection '+JSON.stringify(changed));
  return {before,other,changed};
 } finally {await client.close();}
}
