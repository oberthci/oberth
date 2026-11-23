import {check} from './cdp.mjs';

export async function headerNits(client) {
 const results=[];
 for(const width of [360,760,1024]) {
  await client.cdp('Emulation.setDeviceMetricsOverride',{width,height:900,deviceScaleFactor:1,mobile:false});
  for(const theme of ['light','dark','mono','color'])for(const held of [false,true]) {
   const result=await client.evaluate(`(async()=>{
    document.documentElement.dataset.theme=${JSON.stringify(theme)};
    const held=${held},run={...headerRuns[1],status:held?'running':'passed'},header=document.querySelector('.run-head');
    const actions=runActions(screen,run,[],held?[{step:'test',held_since:run.created_at,remaining_ms:60000}]:[]);
    header.replaceChildren(...runHeader(run,actions,false));
    if(header.nextElementSibling?.matches('.toolbar'))header.nextElementSibling.remove();
    header.after(actions);fitRunHeader(header.firstElementChild);
    const region=header.querySelector('.run-metadata'),origin=region.querySelector('[aria-label="Started by"]'),identity=region.querySelector('.identity'),time=region.querySelector('time').parentElement;
    const dotGap=(before,after)=>{const text=after.firstChild,range=document.createRange();range.setStart(text,text.textContent.indexOf('·'));range.setEnd(text,text.textContent.indexOf('·')+1);return range.getBoundingClientRect().left-before.getBoundingClientRect().right;};
    return {heading:header.querySelector('h1').textContent,headingButtons:header.querySelector('h1').querySelectorAll('button').length,
     gap:actions.getBoundingClientRect().top-header.getBoundingClientRect().bottom,font:getComputedStyle(actions).fontSize,buttons:Array.from(actions.querySelectorAll('button')).map(node=>node.textContent),
     sizes:Array.from(header.querySelectorAll('.state,.run-summary .duration,.run-counts')).map(node=>getComputedStyle(node).fontSize),
     commitGap:!identity.hidden&&!origin.hidden?dotGap(identity.lastElementChild,origin):null,authorGap:!origin.hidden&&!time.hidden?dotGap(origin,time):null};
   })()`);
   check(result.heading===(held?'running ':'passed ')&&result.headingButtons===0,'heading contains an action '+JSON.stringify({width,theme,result}));
   check(result.gap>=8&&result.font==='12px'&&result.buttons.includes(held?'Release':'Rerun'),'action toolbar touches header or loses action '+JSON.stringify({width,theme,result}));
   check(new Set(result.sizes).size===1,'duration or counts differ in font size '+JSON.stringify({width,theme,result}));
   if(width===1024)check(result.commitGap>=4&&result.authorGap>=4,'separator lacks leading space '+JSON.stringify({theme,result}));
   results.push({kind:'nits',width,theme,held,...result});
  }
 }
 return results;
}
