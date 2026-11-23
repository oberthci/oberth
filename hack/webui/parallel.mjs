import {writeFile} from 'node:fs/promises';
import {check} from './cdp.mjs';

export async function parallel(client){
 const results=[];
 for(const width of [1024,760,360]){
  await client.cdp('Emulation.setDeviceMetricsOverride',{width,height:900,deviceScaleFactor:1,mobile:false});
  await client.loaded('/runs/88888888','.steps a.row');
  const split=await client.evaluate(`(()=>{
   const probe=h('div',{class:'steps'});
   probe.append(...stepRows({},[1,2,2,0,1].map((parallel_group,index)=>({parallel_group,plan_path:'probe-'+index,name:'Probe '+index,status:'passed'})),'',null));
   document.body.append(probe);
   const result={rails:probe.querySelectorAll('.parallel-rail').length,grouped:Array.from(probe.querySelectorAll('.row')).map(row=>Boolean(row.closest('.parallel-steps')))};
   probe.remove();return result;
  })()`);
  check(split.rails===1&&JSON.stringify(split.grouped)==='[false,true,true,false,false]','single-row fragments carry a rail '+JSON.stringify({width,split}));
  for(const theme of ['light','dark','mono','color']){
   await client.evaluate(`document.documentElement.dataset.theme=${JSON.stringify(theme)};document.getAnimations().forEach(animation=>animation.cancel());`);
   const metrics=await client.evaluate(`(()=>{
    const rows=Array.from(document.querySelectorAll('.steps .row')),boxes=Array.from(document.querySelectorAll('.parallel-steps'));
    const luminance=colour=>colour.match(/[0-9.]+/g).slice(0,3).map(value=>{const n=Number(value)/255;return n<=0.04045?n/12.92:((n+0.055)/1.055)**2.4;}).reduce((sum,n,index)=>sum+n*[0.2126,0.7152,0.0722][index],0);
    const panel=getComputedStyle(document.querySelector('.steps'));
    const contrast=colour=>{const a=luminance(colour),b=luminance(panel.backgroundColor);return (Math.max(a,b)+0.05)/(Math.min(a,b)+0.05);};
    const rails=boxes.map(box=>{const rect=box.getBoundingClientRect(),rail=box.querySelector('.parallel-rail'),line=rail.getBoundingClientRect();return {contrast:contrast(getComputedStyle(rail).backgroundColor),colour:getComputedStyle(rail).backgroundColor,panel:panel.backgroundColor,border:panel.borderLeftColor,top:line.top,bottom:line.bottom,width:line.width,insetTop:line.top-rect.top,insetBottom:rect.bottom-line.bottom,members:Array.from(box.querySelectorAll('.row')).map(row=>row.dataset.key)};});
    return {overflow:document.documentElement.scrollWidth>innerWidth,groups:boxes.length,rails,
     rows:rows.map(row=>{
      const names=['.state','.step-name','.duration'],rect=row.getBoundingClientRect();
      return {height:rect.height,nowrap:getComputedStyle(row).whiteSpace,
       text:names.map(name=>{const node=row.querySelector(name),range=document.createRange();range.selectNodeContents(node);const box=range.getBoundingClientRect();return {height:box.height,bottom:box.bottom,size:getComputedStyle(node).fontSize};}).filter(text=>text.height>0),
       nameLeft:row.querySelector('.step-name').getBoundingClientRect().left,
       corner:getComputedStyle(row).borderBottomLeftRadius,current:row.getAttribute('aria-current'),status:row.querySelector('.state').textContent.trim(),key:row.dataset.key,group:boxes.indexOf(row.parentElement)};
     })};
   })()`);
   check(!metrics.overflow&&metrics.groups===8&&metrics.rows.length===44&&JSON.stringify(metrics.rails.map(rail=>rail.members.length))==='[3,4,10,6,2,3,3,3]'&&metrics.rails[0].members.join(',')==='release-index,tools-dir,deprecations','wrong parallel members '+JSON.stringify({width,theme,metrics}));
   check(metrics.rails.every(rail=>rail.width===1&&rail.bottom>rail.top&&rail.insetTop>=4&&rail.insetBottom>=4)&&metrics.rails.every((rail,index)=>!index||rail.top-metrics.rails[index-1].bottom>=8),'adjacent rails do not have visible ends '+JSON.stringify({width,theme,metrics}));
   check(metrics.rows.every(row=>row.height===37&&row.nowrap==='nowrap'&&row.text.every(text=>text.height<=22&&text.size===row.text[0].size)&&Math.max(...row.text.map(text=>text.bottom))-Math.min(...row.text.map(text=>text.bottom))<1)&&metrics.rows.every(row=>Math.abs(row.nameLeft-metrics.rows[0].nameLeft)<0.5),'step rows wrap or differ in text level '+JSON.stringify({width,theme,metrics}));
   check(metrics.rails.every(rail=>rail.contrast>=3&&rail.colour!==rail.border),'rail contrast below 3:1 '+JSON.stringify({width,theme,rails:metrics.rails}));
   check(metrics.rows.filter(row=>row.group>=0).every(row=>row.corner==='0px'),'group corners round inside the list '+JSON.stringify({width,theme,metrics}));
   check(metrics.rows.slice(0,6).concat(metrics.rows.slice(-4)).every(row=>row.group===-1),'serial chain carries a rail '+JSON.stringify(metrics));
   const chain=['link-go','install-systemd-tools','scan-systemd-tools'].map(key=>metrics.rows.find(row=>row.key===key));
   check(new Set(chain.map(row=>row.group)).size===3&&chain[1].current==='step'&&chain[1].status==='failed'&&chain[2].status==='pending','dependent steps share a rail '+JSON.stringify(chain));
   const {cssContentSize}=await client.cdp('Page.getLayoutMetrics');
   const shot=await client.cdp('Page.captureScreenshot',{format:'png',captureBeyondViewport:true,clip:{x:0,y:0,width,height:Math.ceil(cssContentSize.height),scale:1}});
   const path=client.artifacts+'/run-parallel-real-'+theme+'-'+width+'.png';
   await writeFile(path,Buffer.from(shot.data,'base64'));
   results.push({width,theme,...metrics,screenshot:path});
  }
 }
 return results;
}
