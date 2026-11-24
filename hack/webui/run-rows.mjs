import {writeFile} from 'node:fs/promises';
import {check} from './cdp.mjs';

export async function runRows(client) {
 const results=[];
 for(const width of [760,1024,1280]) {
  await client.cdp('Emulation.setDeviceMetricsOverride',{width,height:900,deviceScaleFactor:1,mobile:false});
  await client.loaded('/runs','.runs a.row');
  for(const theme of ['light','dark','mono','color']) {
   const {rows,headings}=await client.evaluate(`(()=>{
    document.documentElement.dataset.theme=${JSON.stringify(theme)};
    const rows=Array.from(document.querySelectorAll('.runs a.row')).map(row=>{
     const style=getComputedStyle(row),ref=row.querySelector('.ref'),message=row.querySelector('.message'),range=document.createRange();range.selectNodeContents(message);
     const text=range.getBoundingClientRect(),box=message.getBoundingClientRect();
     return {height:row.getBoundingClientRect().height,lineHeight:parseFloat(style.lineHeight),message:message.textContent,messageHeight:text.height,messageWidth:text.width,columnWidth:box.width,ref:ref.textContent,refWidth:ref.clientWidth,refTextWidth:ref.scrollWidth,
      overflow:document.documentElement.scrollWidth>innerWidth||row.scrollWidth>row.clientWidth};
    });
    const headings=Array.from(document.querySelectorAll('.runs .head > *')).map(node=>{
     const range=document.createRange();range.selectNodeContents(node);
     const text=range.getBoundingClientRect(),box=node.getBoundingClientRect();
     return {label:node.textContent,width:box.width,textWidth:text.width,left:text.left,right:text.right,boxLeft:box.left,boxRight:box.right};
    });
    return {rows,headings};
   })()`);
   check(rows.some(row=>row.message.length===40),'row fixture lacks a full-length message');
   check(rows.every(row=>!row.overflow&&Math.abs(row.height-37)<0.5&&row.messageHeight<=row.lineHeight&&row.messageWidth<=row.columnWidth+0.5),'run rows wrap or cut their message '+JSON.stringify({width,theme,rows}));
   check(headings.length===5&&headings.every(item=>item.textWidth<=item.width+.5&&item.left>=item.boxLeft-.5&&item.right<=item.boxRight+.5),'runs heading crosses its column '+JSON.stringify({width,theme,headings}));
   if(width>=1024)check(rows.every(row=>row.refTextWidth<=row.refWidth),'wide run ref is cut '+JSON.stringify({width,theme,rows}));
   if(width===760&&['mono','color'].includes(theme)) {
    const shot=await client.cdp('Page.captureScreenshot',{format:'png'});
    await writeFile(`${client.artifacts}/run-rows-${width}-${theme}.png`,Buffer.from(shot.data,'base64'));
   }
   results.push({width,theme,rows,headings});
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
 await client.evaluate(`theme('auto')`);
 return results;
}
