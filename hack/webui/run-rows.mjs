import {writeFile} from 'node:fs/promises';
import {check} from './cdp.mjs';

export async function runRows(client) {
 const results=[];
 const injection=await client.cdp('Page.addScriptToEvaluateOnNewDocument',{source:`
  const rowInitialNow=Date.now,rowFetch=fetch;window.rowScene='baseline';
  window.fetch=async(path,options)=>{
   const response=await rowFetch(path,options);
   if(new URL(path,location.href).pathname!=='/api/runs'||rowScene==='baseline')return response;
   const page=await response.json();page.runs[0].ref='refs/heads/master';
   if(rowScene==='long-finished')Object.assign(page.runs[0],{created_at:'2026-11-16T04:58:00Z',started_at:'2026-11-16T04:58:00Z',finished_at:'2026-11-16T15:00:00Z'});
   page.runs.push({...page.runs[0],id:'55555555',repo:'origin/team/'+'long-repository-'.repeat(4),display_reason:'x'.repeat(40)});
   return new Response(JSON.stringify(page));
  };`});
 try {
  for(const width of [320,339,340,359,360,400,499,500,600,759,760,768,780,798,800,839,840,899,960,1024,1280]) {
   await client.cdp('Emulation.setDeviceMetricsOverride',{width,height:900,deviceScaleFactor:1,mobile:false});
   await client.loaded('/runs','.runs a.row');
   for(const [scene,day] of [['today',16],['yesterday',17],['older',19],['long-finished',19]])for(const theme of ['light','dark','mono','color']) {
    const metrics=await client.evaluate(`(async()=>{
     rowScene=${JSON.stringify(scene)};Date.now=()=>Date.parse('2026-11-${day}T20:22:00Z');
     document.documentElement.dataset.theme=${JSON.stringify(theme)};await screen.read();
     const table=document.querySelector('.runs'),main=document.querySelector('main'),frame=main.getBoundingClientRect(),style=getComputedStyle(main),box=table.getBoundingClientRect();
     const measure=node=>{
      const range=document.createRange();range.selectNodeContents(node);
      const bounds=node.getBoundingClientRect(),lines=Array.from(range.getClientRects()).filter(part=>part.width>0);
      return {text:node.textContent,width:bounds.width,textWidth:range.getBoundingClientRect().width,whole:lines.every(part=>part.left>=bounds.left-.5&&part.right<=bounds.right+.5&&part.left>=box.left-.5&&part.right<=box.right+.5),lines:lines.length};
     };
     const rows=Array.from(table.querySelectorAll('a.row')).map(row=>({height:row.getBoundingClientRect().height,message:measure(row.querySelector('.message')),ref:row.querySelector('.ref').textContent,refWidth:row.querySelector('.ref').clientWidth,refTextWidth:row.querySelector('.ref').scrollWidth,when:measure(row.querySelector('.when')),overflow:row.scrollWidth>row.clientWidth}));
     const visible=getComputedStyle(table.querySelector('.head')).display!=='none';
     const headings=visible?Array.from(table.querySelectorAll('.head > *')).map(measure):[];
     const label=document.querySelector('.filters label'),range=document.createRange();range.setStart(label.firstChild,0);range.setEnd(label.firstChild,10);
     return {rows,headings,themeLabelWidth:range.getBoundingClientRect().width,tableWidth:box.width,frameWidth:frame.width-parseFloat(style.paddingLeft)-parseFloat(style.paddingRight),inside:box.left>=frame.left+parseFloat(style.paddingLeft)-.5&&box.right<=frame.right-parseFloat(style.paddingRight)+.5,pageOverflow:document.documentElement.scrollWidth>innerWidth};
    })()`);
    const context=JSON.stringify({width,scene,theme,...metrics});
    check(!metrics.pageOverflow&&metrics.inside&&metrics.tableWidth<=metrics.frameWidth+.5&&metrics.rows.every(row=>!row.overflow),'run table leaves its frame '+context);
    check(metrics.rows.some(row=>row.message.text.length===40)&&metrics.rows.every(row=>row.message.whole&&row.when.whole),'run message or date is clipped '+context);
    check(metrics.headings.every(item=>item.whole),'runs heading crosses its column '+context);
    if(width===1024&&['mono','color'].includes(theme))check(Math.abs(metrics.headings[1].textWidth-metrics.themeLabelWidth)<.5,'run heading differs from its theme label '+context);
    check(metrics.rows.every(row=>Math.abs(row.height-metrics.rows[0].height)<.5),'run rows have unequal heights '+context);
    if(width>=840)check(metrics.rows.every(row=>Math.abs(row.height-37)<.5),'wide run rows wrap '+context);
    if(width>=500)check(metrics.rows.filter(row=>row.ref==='master').every(row=>row.refTextWidth<=row.refWidth),'six-character run ref is cut '+context);
    if(width>=840)check(metrics.rows.filter(row=>row.ref==='fix-login').every(row=>row.refTextWidth<=row.refWidth),'ordinary run ref lost its space '+context);
    if(width>=1024)check(metrics.rows.every(row=>row.refTextWidth<=row.refWidth),'wide run ref is cut '+context);
    if(scene==='long-finished')check(metrics.rows[0].when.text.length>=21,'dated fixture lacks a long finished WHEN '+context);
    if(scene==='older'&&((width===768&&theme==='light')||(width===840&&['mono','color'].includes(theme)))) {
     const shot=await client.cdp('Page.captureScreenshot',{format:'png'});
     await writeFile(`${client.artifacts}/run-rows-dated-${width}-${theme}.png`,Buffer.from(shot.data,'base64'));
    }
    results.push({width,scene,theme,...metrics});
   }
  }
  for(const [width,value] of [[1024,'light'],[400,'dark']]) {
   await client.cdp('Emulation.setDeviceMetricsOverride',{width,height:900,deviceScaleFactor:1,mobile:false});
   for(const [page,path,selector] of [['runs','/runs','.runs a.row'],['run','/runs/22222222','.log h2']]) {
    await client.loaded(path,selector);
    await client.evaluate(`theme(${JSON.stringify(value)});document.getAnimations().forEach(animation=>animation.cancel())`);
    const shot=await client.cdp('Page.captureScreenshot',{format:'png'});
    await writeFile(`${client.artifacts}/${page}-${width}-${value}.png`,Buffer.from(shot.data,'base64'));
   }
  }
  await client.cdp('Emulation.setDeviceMetricsOverride',{width:1280,height:900,deviceScaleFactor:1,mobile:false});
  await client.evaluate(`Date.now=rowInitialNow;theme('auto')`);
  return results;
 } finally {await client.cdp('Page.removeScriptToEvaluateOnNewDocument',{identifier:injection.identifier});}
}
