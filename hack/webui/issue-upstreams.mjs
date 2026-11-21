import {writeFile} from 'node:fs/promises';
import {launch,check,step} from './cdp.mjs';

let client;
try {
 client=await launch({token:'valid'});
 step('scoped viewer opens administrator issue');
 await client.loaded('/issues', '.issues a.row');
 check(await client.evaluate(`document.querySelector('.issues a.row').textContent.includes('Scoped administrator task')`), 'scoped viewer cannot discover administrator issue');
 await client.loaded('/issues/1', '.issues h2');
 check(await client.evaluate(`document.querySelector('.issues h2').textContent === '#1 Scoped administrator task'`), 'scoped viewer cannot render administrator issue');
 const issue=await client.evaluate(`fetch('/api/issues/1',{headers:{Authorization:'Bearer '+localStorage.getItem('oberth.token')}}).then(async response=>({status:response.status,issue:await response.json()}))`);
 check(issue.status===200 && issue.issue.upstream==='origin', 'scoped viewer cannot read defaulted upstream through API');
 client.checkFatal();
 await writeFile(client.artifacts+'/issue-upstreams.json',JSON.stringify({id:issue.issue.id,upstream:issue.issue.upstream,title:issue.issue.title})+'\n');
 console.log('Issue upstream browser assertions passed.');
} catch(error){console.error('ui-browser: FAIL ('+error.message+')');process.exitCode=1;}
finally{try{await client?.close();}catch(error){console.error('ui-browser: FAIL ('+error.message+')');process.exitCode=1;}}
