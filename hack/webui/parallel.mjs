import {writeFile} from 'node:fs/promises';
import {check} from './cdp.mjs';

export async function parallel(client){
 const results=[];
 await client.loaded('/runs/22222222','.steps a.row');
 const fragments=await client.evaluate(`(()=>{
  const steps=[1,1,0,2,2,1].map((parallel_group,index)=>({parallel_group,plan_path:String(index),name:String(index),status:'pending'}));
  const box=h('div',{},...stepRows({},steps,'',h('section')));
  return Array.from(box.children).map(node=>({group:node.dataset.group||null,paths:Array.from(node.matches('a')?[node]:node.children).map(row=>row.dataset.key)}));
 })()`);
 check(JSON.stringify(fragments)===JSON.stringify([{group:'1',paths:['0','1']},{group:null,paths:['2']},{group:'2',paths:['3','4']},{group:'1',paths:['5']}]),'parallel fragments enclose serial steps or reorder the plan '+JSON.stringify(fragments));
 for(const width of [1024,760,360]){
  await client.cdp('Emulation.setDeviceMetricsOverride',{width,height:900,deviceScaleFactor:1,mobile:false});
  await client.loaded('/runs/22222222','.steps a.row');
  for(const theme of ['light','dark','mono','color']){
   await client.evaluate(`document.documentElement.dataset.theme=${JSON.stringify(theme)};document.getAnimations().forEach(animation=>animation.cancel());`);
   const metrics=await client.evaluate(`(()=>{
    const rows=Array.from(document.querySelectorAll('.steps .row')),boxes=Array.from(document.querySelectorAll('.parallel-steps'));
    const box=boxes[0],style=box&&getComputedStyle(box);
    return {overflow:document.documentElement.scrollWidth>innerWidth,groups:boxes.length,
     members:box&&Array.from(box.children).map(row=>row.dataset.key),
     serial:rows.slice(0,2).every(row=>row.parentElement.matches('.steps')),
     border:style&&[style.borderLeftWidth,style.borderLeftStyle,style.borderLeftColor],
     height:box&&box.getBoundingClientRect().height,
     rows:rows.map(row=>{
      const names=['.state','.step-name','.duration'],rect=row.getBoundingClientRect();
      return {height:rect.height,nowrap:getComputedStyle(row).whiteSpace,
       text:names.map(name=>{const node=row.querySelector(name),range=document.createRange();range.selectNodeContents(node);const box=range.getBoundingClientRect();return {height:box.height,bottom:box.bottom,size:getComputedStyle(node).fontSize};}),
       nameLeft:row.querySelector('.step-name').getBoundingClientRect().left,
       current:row.getAttribute('aria-current'),status:row.querySelector('.state').textContent.trim()};
     })};
   })()`);
   check(!metrics.overflow&&metrics.groups===1&&metrics.serial&&JSON.stringify(metrics.members)==='["x-test","w-publish"]','parallel rows do not share one border, or serial rows are grouped '+JSON.stringify({width,theme,metrics}));
   check(metrics.border[0]==='1px'&&metrics.border[1]==='solid'&&!['transparent','rgba(0, 0, 0, 0)'].includes(metrics.border[2])&&metrics.height===74,'parallel border is not visible or continuous '+JSON.stringify({width,theme,metrics}));
   check(metrics.rows.every(row=>row.height===37&&row.nowrap==='nowrap'&&row.text.every(text=>text.height<=22&&text.size===row.text[0].size)&&Math.max(...row.text.map(text=>text.bottom))-Math.min(...row.text.map(text=>text.bottom))<1)&&metrics.rows.every(row=>Math.abs(row.nameLeft-metrics.rows[0].nameLeft)<0.5),'step rows wrap or differ in text level '+JSON.stringify({width,theme,metrics}));
   check(metrics.rows[2].current==='step'&&metrics.rows[2].status==='failed'&&metrics.rows[3].status==='passed','parallel success after failure is missing '+JSON.stringify(metrics));
   const shot=await client.cdp('Page.captureScreenshot',{format:'png',captureBeyondViewport:false});
   const path=client.artifacts+'/run-parallel-'+theme+'-'+width+'.png';
   await writeFile(path,Buffer.from(shot.data,'base64'));
   results.push({width,theme,...metrics,screenshot:path});
  }
 }
 return results;
}
