import assert from 'node:assert/strict';
import {spawn} from 'node:child_process';
import {mkdir} from 'node:fs/promises';
import {once} from 'node:events';
import {writeFileSync} from 'node:fs';

export const check = (condition, message) => assert.ok(condition, message);
export const pause = milliseconds => new Promise(resolve => setTimeout(resolve, milliseconds));
let currentStep='launch browser';
export function step(label) {
 currentStep=label;
 if(process.env.OBERTH_UI_STEP_FILE)writeFileSync(process.env.OBERTH_UI_STEP_FILE,label);
}
export async function deadline(label, task, milliseconds=5000) {
 const previous=currentStep;
 step(label);
 let timer;
 try{return await Promise.race([task(),new Promise((_,reject)=>{timer=setTimeout(()=>reject(new Error(label+' timed out')),milliseconds);})]);}
 finally{clearTimeout(timer);step(previous);}
}

export async function launch(options = {}) {
 const artifacts = options.artifacts || process.env.OBERTH_UI_ARTIFACTS;
 await mkdir(artifacts, {recursive:true});
 const token = options.token || 'ui-example-credential';
 const server = spawn(process.env.OBERTH_UI_BINARY, ['-test.run=^TestUIExampleServer$', '-test.v'], {
  env:{...process.env,...options.env,OBERTH_UI_EXAMPLE:'127.0.0.1:0',OBERTH_UI_TOKEN:token},stdio:['ignore','pipe','pipe'],
 });
 const serverExit = once(server,'exit');
 let closing = false, browser, ws;
 try {
  const origin = await new Promise((resolve,reject) => {
   server.stdout.on('data', chunk => {const match=String(chunk).match(/Fixed examples at (http:\/\/127\.0\.0\.1:\d+)/);if(match)resolve(match[1]);});
   server.once('exit', code => reject(new Error('example server exited before listening: '+code)));
  });
  browser = spawn(process.env.CHROMIUM_BIN, ['--headless','--no-sandbox','--disable-dev-shm-usage','--disable-gpu','--no-first-run','--no-default-browser-check','--disable-background-networking','--disable-component-update','--disable-sync','--metrics-recording-only','--window-size=1280,900','--remote-debugging-port=0','--user-data-dir='+artifacts+'/profile','about:blank'],{stdio:['ignore','ignore','pipe']});
  const address = await new Promise((resolve,reject) => {
   browser.stderr.on('data', chunk => {const match=String(chunk).match(/DevTools listening on (ws:\/\/\S+)/);if(match)resolve(match[1]);});
   browser.once('exit', code => reject(new Error('browser exited before listening: '+code)));
  });
  ws = new WebSocket(address);await once(ws,'open');
  let serial=0, fatalError;
  const pending=new Map(), events=[], waiters=[], requests=[];
  let fail;
  const fatal = new Promise((_,reject)=>{fail=error=>{fatalError=error;reject(error);};});
  fatal.catch(()=>{});
  ws.addEventListener('message',event=>{
   const value=JSON.parse(event.data);
   if(value.id){const item=pending.get(value.id);pending.delete(value.id);if(item)value.error?item.reject(value.error):item.resolve(value.result);return;}
   events.push(value);
   if(value.method==='Network.requestWillBeSent'){
    const request=value.params.request,url=new URL(request.url);
    const leaked=url.href.includes(token)||[...url.searchParams.keys()].some(key=>/token|authorization/i.test(key));
    if(leaked)fail(new assert.AssertionError({message:'credential in a request URL'}));
    requests.push({method:request.method,path:url.pathname,url:leaked?'[redacted]':url.href,at:performance.now(),body:request.postData});
   }
   for(const waiter of [...waiters])if(waiter.method===value.method&&waiter.sessionId===value.sessionId){waiters.splice(waiters.indexOf(waiter),1);waiter.resolve(value.params);}
  });
  const send=(method,params={},sessionId)=>new Promise((resolve,reject)=>{const id=++serial;pending.set(id,{resolve,reject});ws.send(JSON.stringify({id,method,params,...(sessionId?{sessionId}:{})}));});
  const {targetId}=await send('Target.createTarget',{url:'about:blank'});
  const {sessionId}=await send('Target.attachToTarget',{targetId,flatten:true});
  const cdp=(method,params={})=>send(method,params,sessionId);
  const nextEvent=method=>Promise.race([new Promise(resolve=>waiters.push({method,sessionId,resolve})),fatal]);
  const evaluate=async expression=>{
   const result=await Promise.race([cdp('Runtime.evaluate',{expression,awaitPromise:true,returnByValue:true}),fatal]);
   if(result.exceptionDetails)throw new Error(JSON.stringify(result.exceptionDetails));
   return result.result.value;
  };
  for(const domain of ['Page','Runtime','Network'])await cdp(domain+'.enable');
  await cdp('Emulation.setTimezoneOverride',{timezoneId:'Europe/Amsterdam'});
  const injection=await cdp('Page.addScriptToEvaluateOnNewDocument',{source:`if(!sessionStorage.getItem('ui-example-initialized')){localStorage.setItem('oberth.token',${JSON.stringify(token)});sessionStorage.setItem('ui-example-initialized','1');}Date.now=()=>Date.parse('2026-11-16T20:22:00Z');window.uiFetches=[];const nativeFetch=window.fetch;window.fetch=(path,options={})=>{window.uiFetches.push({path:new URL(path,location.href).pathname,credentials:options.credentials,redirect:options.redirect,authorization:Boolean(options.headers?.Authorization)});return nativeFetch(path,options);};`});
  async function loaded(path,selector){
   try { await deadline('load '+path,async()=>{
    const ready=nextEvent('Page.loadEventFired');await cdp('Page.navigate',{url:origin+path});await ready;
    await evaluate(`new Promise(resolve=>{const test=()=>{if(!document.querySelector(${JSON.stringify(selector)}))return;observer.disconnect();resolve();};const observer=new MutationObserver(test);observer.observe(document,{subtree:true,childList:true});test();})`);
   }); } catch(error) {
    const page=await evaluate(`({notice:document.querySelector('#notice')?.textContent,main:document.querySelector('main')?.textContent,signin:document.querySelector('#signin')?.checkVisibility()})`);
    throw new Error(error.message+' '+JSON.stringify(page));
   }
  }
  async function close(){
   if(closing)return;closing=true;
   step('close browser and example server');
   try{await deadline('close browser',()=>send('Browser.close'));}
   finally{ws.close();browser.kill('SIGTERM');server.kill('SIGTERM');await deadline('close example server',()=>serverExit);}
  }
  return {artifacts,origin,token,cdp,evaluate,nextEvent,loaded,events,requests,injection,pauseServer:paused=>server.kill(paused?'SIGSTOP':'SIGCONT'),checkFatal:()=>{if(fatalError)throw fatalError;},close};
 }catch(error){closing=true;ws?.close();browser?.kill('SIGTERM');server.kill('SIGTERM');await serverExit;throw error;}
}
