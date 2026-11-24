import {check, launch} from './cdp.mjs';

export async function statusSettings(client) {
 const results=[];
 await client.loaded('/runs','#main');
 await client.evaluate(`localStorage.setItem('oberth.token',${JSON.stringify(client.token)})`);
 for (const width of [1024,360]) {
  await client.cdp('Emulation.setDeviceMetricsOverride',{width,height:900,deviceScaleFactor:1,mobile:false});
  for (const theme of ['light','dark','mono','color']) {
   await client.evaluate(`localStorage.setItem('oberth.theme',${JSON.stringify(theme)})`);
   await client.loaded('/status','.setting-row');
   const result=await client.evaluate(`(async()=>{
    const status=await (await fetch('/api/status',{headers:{Authorization:'Bearer '+localStorage.getItem('oberth.token')}})).json();
    return {expected:status.settings.map(s=>[s.name,s.source,s.value]), theme:document.documentElement.dataset.theme,
     rows:Array.from(document.querySelectorAll('.setting-row')).map(row=>({cells:Array.from(row.children).map(cell=>cell.textContent),height:row.getBoundingClientRect().height,markerRight:row.children[1].getBoundingClientRect().right,whiteSpace:getComputedStyle(row).whiteSpace,ellipsis:Array.from(row.children).some(cell=>getComputedStyle(cell).textOverflow==='ellipsis')})),
     scroll:document.documentElement.scrollWidth};
   })()`);
   check(result.theme===theme&&result.expected.length>=40,'missing settings or theme');
   check(JSON.stringify(result.expected)===JSON.stringify(result.rows.map(row=>row.cells)),'a setting, full value or origin was lost');
   check(result.scroll<=width,'settings overflow the page');
   check(result.rows.every(row=>row.markerRight<=width),'origin markers are outside the viewport');
   check(result.rows.every(row=>row.height===result.rows[0].height&&row.height<=36&&row.whiteSpace==='nowrap'&&!row.ellipsis),'settings rows wrap, truncate or have different heights');
   results.push({width,theme,rows:result.rows.length});
  }
 }
 return results;
}

if (process.argv.includes('--standalone')) {
 let client;
 try { client=await launch(); console.log(JSON.stringify(await statusSettings(client))); client.checkFatal(); }
 finally { await client?.close(); }
}
