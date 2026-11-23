import {check} from './cdp.mjs';

export async function metadata(client) {
 const results=[];
 for(const width of [360,760,960,1024]) {
  await client.cdp('Emulation.setDeviceMetricsOverride',{width,height:900,deviceScaleFactor:1,mobile:false});
  for(const theme of ['light','dark','mono','color'])for(const ref of ['refs/heads/feature/search','refs/tags/v1.2.3']) {
   const result=await client.evaluate(`(async()=>{
    document.documentElement.dataset.theme=${JSON.stringify(theme)};
    const run={...headerRuns[1],ref:${JSON.stringify(ref)}},header=document.querySelector('.run-head');
    header.replaceChildren(...runHeader(run,runActions(screen,run,[],[]),false));
    await new Promise(resolve=>requestAnimationFrame(()=>requestAnimationFrame(resolve)));
    const region=header.querySelector('.run-metadata'),refNode=region.firstElementChild,rect=refNode.getBoundingClientRect(),meta=header.querySelector('.run-meta');
    return {text:refNode.innerText,first:refNode.className,width:rect.width,left:rect.left,right:rect.right,metaRight:meta.getBoundingClientRect().right,
     identity:!region.querySelector('.identity').hidden,origin:!region.querySelector('[aria-label="Started by"]').hidden,time:!region.querySelector('time').parentElement.hidden,
     overflow:document.documentElement.scrollWidth>innerWidth||meta.scrollWidth>meta.clientWidth,height:meta.getBoundingClientRect().height,lineHeight:parseFloat(getComputedStyle(meta).lineHeight)};
   })()`);
   const label=ref.startsWith('refs/tags/')?'tag v1.2.3':'feature/search';
   check(result.first==='run-ref'&&result.text==='origin/team/web · '+label&&result.width>0&&!result.overflow&&result.right<=result.metaRight,'repository/ref hidden, cut or not first '+JSON.stringify({width,theme,ref,result}));
   check(result.height<=result.lineHeight,'metadata wraps '+JSON.stringify({width,theme,result}));
   if(width>=760)check(result.origin&&result.time,'author/trigger/time hidden despite available width '+JSON.stringify({width,theme,result}));
   if(width===1024)check(result.identity,'id/commit hidden at 1024 '+JSON.stringify({theme,result}));
   results.push({kind:'metadata',width,theme,ref,...result});
  }
 }
 return results;
}
