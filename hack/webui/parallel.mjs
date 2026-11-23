import {writeFile} from 'node:fs/promises';
import {check} from './cdp.mjs';

export async function parallel(client){
 const results=[];
 for(const width of [1024,760,360]){
  await client.cdp('Emulation.setDeviceMetricsOverride',{width,height:900,deviceScaleFactor:1,mobile:false});
  await client.loaded('/runs/22222222','.steps a.row');
  for(const theme of ['light','dark','mono','color']){
   await client.evaluate(`document.documentElement.dataset.theme=${JSON.stringify(theme)};document.getAnimations().forEach(animation=>animation.cancel());`);
   const metrics=await client.evaluate(`(()=>{
    const rows=Array.from(document.querySelectorAll('.steps .row')),boxes=Array.from(document.querySelectorAll('.parallel-steps'));
    const rails=boxes.map(box=>{const rect=box.getBoundingClientRect(),rail=box.querySelector('.parallel-rail'),line=rail.getBoundingClientRect();return {top:line.top,bottom:line.bottom,width:line.width,insetTop:line.top-rect.top,insetBottom:rect.bottom-line.bottom,members:Array.from(box.querySelectorAll('.row')).map(row=>row.dataset.key)};});
    return {overflow:document.documentElement.scrollWidth>innerWidth,groups:boxes.length,rails,
     rows:rows.map(row=>{
      const names=['.state','.step-name','.duration'],rect=row.getBoundingClientRect();
      return {height:rect.height,nowrap:getComputedStyle(row).whiteSpace,
       text:names.map(name=>{const node=row.querySelector(name),range=document.createRange();range.selectNodeContents(node);const box=range.getBoundingClientRect();return {height:box.height,bottom:box.bottom,size:getComputedStyle(node).fontSize};}),
       nameLeft:row.querySelector('.step-name').getBoundingClientRect().left,
       current:row.getAttribute('aria-current'),status:row.querySelector('.state').textContent.trim()};
     })};
   })()`);
   check(!metrics.overflow&&metrics.groups===2&&JSON.stringify(metrics.rails.map(rail=>rail.members))==='[["z-checkout","y-build"],["x-test","w-publish"]]','wrong parallel members '+JSON.stringify({width,theme,metrics}));
   check(metrics.rails.every(rail=>rail.width===1&&rail.bottom>rail.top&&rail.insetTop>=4&&rail.insetBottom>=4)&&metrics.rails[1].top-metrics.rails[0].bottom>=8,'adjacent rails do not have visible ends '+JSON.stringify({width,theme,metrics}));
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
