import {check} from './cdp.mjs';

export async function containersFit(client) {
 const results=[];
 for(const width of [400,600,1024]) {
  await client.cdp('Emulation.setDeviceMetricsOverride',{width,height:900,deviceScaleFactor:1,mobile:false});
  await client.loaded('/runs/22222222','.log select');
  for(const value of ['light','dark','mono','color']) {
   const result=await client.evaluate(`(()=>{
    document.documentElement.dataset.theme=${JSON.stringify(value)};
    const select=document.querySelector('.log select'),name='workflow-1234567890123456789012345678901234567890-1234567890 · main';
    if(!Array.from(select.options).some(option=>option.textContent===name))select.append(h('option',{value:'long'},name));
    const box=select.getBoundingClientRect(),panel=document.querySelector('.log').getBoundingClientRect();
    return {left:box.left,right:box.right,panelRight:panel.right,width:box.width,scroll:document.documentElement.scrollWidth,inner:innerWidth,option:select.lastElementChild.textContent};
   })()`);
   check(result.left>=0&&result.width>0&&result.right<=result.panelRight+0.5&&result.scroll<=result.inner&&result.option.endsWith(' · main'),'Container select overflows '+JSON.stringify({width,value,result}));
   results.push({width,theme:value,...result});
  }
 }
 return results;
}
