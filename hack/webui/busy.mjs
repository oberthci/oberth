import {launch,check,step} from './cdp.mjs';

// A busy server: the running example run emits an event every second. With the
// Runs page and then the run page open, every request is answered, the page
// aborts none of its own requests, and no notice appears.
export async function busy(){
 process.env.OBERTH_UI_EVENTS='1';
 const client=await launch();
 const results=[];
 try{
  for(const [path,selector] of [['/runs','.runs a.row'],['/runs/33333333','.steps a.row']]){
   step('busy server '+path);
   await client.loaded(path,selector);
   await client.evaluate(`window.uiBusy=[];new MutationObserver(()=>{if(notice.textContent)uiBusy.push(notice.textContent);}).observe(notice,{subtree:true,childList:true,characterData:true});`);
   const events=client.events.length,requests=client.requests.length;
   await new Promise(resolve=>setTimeout(resolve,5000));
   const notices=await client.evaluate(`[...new Set(uiBusy),notice.textContent].filter(Boolean)`);
   const failed=client.events.slice(events).filter(event=>event.method==='Network.loadingFailed').map(event=>event.params.errorText);
   const waits=client.requests.slice(requests).filter(request=>request.path==='/mcp'&&request.body?.includes('events_wait')).length;
   const reads=client.requests.slice(requests).filter(request=>request.path===(path==='/runs'?'/api/runs':'/api/runs/33333333')).length;
   results.push({path,notices,failed,waits,reads});
   check(!notices.length&&!failed.length&&waits>=3&&reads>=3,'busy server: a request failed or a notice appeared '+JSON.stringify(results.at(-1)));
  }
 }finally{await client.close();delete process.env.OBERTH_UI_EVENTS;}
 return results;
}
