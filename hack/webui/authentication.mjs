import {check,pause,deadline} from './cdp.mjs';

export async function signout(client){
 await client.loaded('/runs','.runs a.row');
 const headerState=()=>client.evaluate(`({nav:document.querySelector('nav').getBoundingClientRect().height>0,signout:document.querySelector('#signout').getBoundingClientRect().height>0,header:document.querySelector('header').getBoundingClientRect().height>0})`);
 const signedIn=await headerState();
 check(signedIn.nav&&signedIn.signout&&signedIn.header,'signed-in navigation or Sign out missing');
 await deadline('load after sign-out',async()=>{const ready=client.nextEvent('Page.loadEventFired');await client.evaluate(`document.querySelector('#signout').click()`);await ready;});await pause(100);
 const result=await client.evaluate(`({hasToken:localStorage.getItem('oberth.token')!==null,form:!signin.hidden,mainHidden:main.hidden,notice:notice.textContent})`);
 check(!result.hasToken,'sign-out retained the bearer token');
 check(result.form&&result.mainHidden&&result.notice==='', 'first visit did not show a quiet sign-in form');
 const signedOut=await headerState();
 check(!signedOut.nav&&!signedOut.signout&&!signedOut.header,'signed-out page shows navigation or Sign out');
 const checking=await client.evaluate(`(async()=>{
  const original=fetch;let release;const answer=new Promise(resolve=>{release=resolve;});
  window.fetch=async(...args)=>{await answer;return original(...args);};
  localStorage.setItem('oberth.token','invalid');
  try{
   const pending=route(),shown=document.querySelector('header').getBoundingClientRect().height>0;
   release();await pending;return shown;
  }finally{release();window.fetch=original;}
 })()`);
 check(!checking,'stored but refused bearer flashed the header before its answer');
 await pause(100);
 check((await client.evaluate(`notice.textContent`)).includes('credentials'), 'invalid credentials did not show their refusal');
 const refused=await headerState();
 check(!refused.nav&&!refused.signout&&!refused.header,'refused sign-in shows navigation or Sign out');
 await deadline('load after sign-in',async()=>{
  const ready=client.nextEvent('Page.loadEventFired');
  await client.evaluate(`signin.querySelector('input').value=${JSON.stringify(client.token)};signin.querySelector('form').requestSubmit()`);
  await ready;
  await client.evaluate(`new Promise(resolve=>{const test=()=>{if(document.querySelector('header').hidden||!document.querySelector('.runs a.row'))return;observer.disconnect();resolve();};const observer=new MutationObserver(test);observer.observe(document,{subtree:true,childList:true,attributes:true});test();})`);
 });
 const restored=await headerState();
 check(restored.nav&&restored.signout&&restored.header,'sign-in did not restore navigation and Sign out');
 return {...result,signedIn,signedOut,checking,refused,restored,certificate:await certificateSignin(client)};
}

// Model a server accepting a browser certificate without a stored bearer.
async function certificateSignin(client){
 const result=await client.evaluate(`(async()=>{
  const original=fetch,stored=localStorage.getItem('oberth.token'),requests=[];
  screen.controller.abort();localStorage.removeItem('oberth.token');
  document.querySelector('header').hidden=true;signin.hidden=false;main.hidden=true;
  history.replaceState(null,'','/repos');
  window.fetch=(path,options)=>{
   if(path.startsWith('/api/repos')){
    requests.push({path,authorization:Boolean(options.headers?.Authorization)});
    return Promise.resolve(new Response(JSON.stringify({repositories:[]})));
   }
   return new Promise((_,reject)=>options.signal.addEventListener('abort',()=>reject(new DOMException('Aborted','AbortError')),{once:true}));
  };
  try{
   await route();
   return {stored:localStorage.getItem('oberth.token')!==null,requests,header:document.querySelector('header').getBoundingClientRect().height>0,form:!signin.hidden,mainHidden:main.hidden,text:main.textContent};
  }finally{screen.controller.abort();window.fetch=original;localStorage.setItem('oberth.token',stored);}
 })()`);
 check(!result.stored&&result.requests.length===1&&!result.requests[0].authorization,'certificate sign-in did not make its normal request without a stored bearer');
 check(result.header&&!result.form&&!result.mainHidden&&result.text.includes('No repositories yet.'),'certificate-accepted answer did not show the application');
 return result;
}
