import {check} from './cdp.mjs';

export async function selectedWorkflow(client) {
 await client.loaded('/runs','.runs a.row');
 const result=await client.evaluate(`(async()=>{
  const original=fetch,file='hack/walk-vm.yaml';
  window.fetch=async(path,options)=>{
   const response=await original(path,options);
   if(!path.startsWith('/api/runs?'))return response;
   const page=await response.json();page.runs[0].selected_file=file;
   return new Response(JSON.stringify(page),{headers:{'Content-Type':'application/json'}});
  };
  try{
   await screen.read();
   const listed=document.querySelector('.runs a.row .message').textContent;
   const run=await get('/api/runs/11111111');run.selected_file=file;
   const box=h('section');box.append(...runHeader(run,h('div'),false));
   return {listed,detail:box.querySelector('.run-file')?.textContent,
    plain:runMessage({display_reason:'step failed'}),selected:runMessage({selected_file:file,display_reason:'step failed'})};
  }finally{window.fetch=original;}
 })()`);
 check(result.listed.includes('hack/walk-vm.yaml selected'),'run list hides the selected workflow');
 check(result.plain==='step failed' && result.selected==='hack/walk-vm.yaml selected · step failed','selection changed the existing run reason');
 check(result.detail==='Workflow: hack/walk-vm.yaml','run detail hides the selected workflow');
 return result;
}
