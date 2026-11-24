import {check} from './cdp.mjs';
import {writeFile} from 'node:fs/promises';

export async function metadata(client) {
 const results=[];
 const cases=[
  {name:'branch',repo:'origin/team/web',ref:'refs/heads/feature/search'},
  {name:'tag',repo:'origin/team/web',ref:'refs/tags/v1.2.3'},
  {name:'mac',repo:'192-168-86-247/repos/oberth-smoke',ref:'refs/heads/smoke-7'},
  {name:'long',repo:'origin/team/web',ref:'refs/heads/feature/'+'long-branch-'.repeat(6)+'check'},
 ];
 for(const width of [360,400,420,600,760,960,1024]) {
  await client.cdp('Emulation.setDeviceMetricsOverride',{width,height:900,deviceScaleFactor:1,mobile:false});
  for(const theme of ['light','dark','mono','color'])for(const example of cases) {
   const result=await client.evaluate(`(async()=>{
    document.documentElement.dataset.theme=${JSON.stringify(theme)};
    const run={...headerRuns[1],repo:${JSON.stringify(example.repo)},ref:${JSON.stringify(example.ref)}},header=document.querySelector('.run-head');
    header.replaceChildren(...runHeader(run,runActions(screen,run,[],[]),false));
    fitRunHeader(header.firstElementChild);
    const region=header.querySelector('.run-metadata'),refNode=region.firstElementChild,rect=refNode.getBoundingClientRect(),meta=header.querySelector('.run-meta');
    const range=document.createRange();range.selectNodeContents(refNode);
    const fragments=Array.from(range.getClientRects());
    return {text:refNode.innerText,title:refNode.title,first:refNode.className,width:rect.width,left:rect.left,right:rect.right,metaRight:meta.getBoundingClientRect().right,
     whole:refNode.scrollWidth<=refNode.clientWidth&&fragments.every(part=>part.width>0&&part.left>=rect.left-.5&&part.right<=rect.right+.5&&part.top>=rect.top-.5&&part.bottom<=rect.bottom+.5),refLines:fragments.length,
     identity:region.querySelector('[title="Run ID"]').checkVisibility(),id:region.querySelector('[title="Run ID"]').textContent,idHeight:region.querySelector('[title="Run ID"]').getBoundingClientRect().height,commit:region.querySelector('[title="Commit ID"]').checkVisibility(),origin:!region.querySelector('[aria-label="Started by"]').hidden,time:!region.querySelector('time').parentElement.hidden,
     overflow:document.documentElement.scrollWidth>innerWidth||meta.scrollWidth>meta.clientWidth,height:meta.getBoundingClientRect().height,lineHeight:parseFloat(getComputedStyle(meta).lineHeight)};
   })()`);
   const label=example.ref.replace('refs/heads/','').replace('refs/tags/','tag ');
   check(result.first==='run-ref'&&result.text===example.repo+' · '+label&&result.title===result.text&&result.width>0&&result.whole&&!result.overflow&&result.right<=result.metaRight,'repository/ref text is clipped '+JSON.stringify({width,theme,example,result}));
   check(result.height<=result.lineHeight*(result.refLines+1),'metadata exceeds its text and identity lines '+JSON.stringify({width,theme,result}));
   check(result.identity&&result.id==='run '+(await client.evaluate(`headerRuns[1].id.slice(0,8)`))&&result.idHeight>0,'run id hidden on row 2 '+JSON.stringify({width,theme,result}));
   if(width>=960&&['branch','tag'].includes(example.name))check(result.origin&&result.time,'author/trigger/time hidden despite available width '+JSON.stringify({width,theme,result}));
   if(width===1024&&['branch','tag'].includes(example.name))check(result.commit,'commit hidden at 1024 '+JSON.stringify({theme,result}));
   if(theme==='light'&&((example.name==='mac'&&width===420)||(example.name==='long'&&width===600))) {
    const height=await client.evaluate(`Math.ceil(document.querySelector('.run-head').getBoundingClientRect().bottom)`);
    const shot=await client.cdp('Page.captureScreenshot',{format:'png',clip:{x:0,y:0,width,height,scale:1}});
    await writeFile(`${client.artifacts}/header-${example.name}-${width}-${theme}.png`,Buffer.from(shot.data,'base64'));
   }
   results.push({kind:'metadata',width,theme,...example,...result});
  }
 }
 return results;
}
