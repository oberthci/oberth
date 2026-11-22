import {writeFile} from 'node:fs/promises';
import {launch,step} from './cdp.mjs';
import {behavior} from './behavior.mjs';
import {captures} from './captures.mjs';
import {busy} from './busy.mjs';
import {repositories} from './repositories.mjs';

let client;
try {
 client=await launch();
 step('browser behavior assertions');
 const result={behavior:await behavior(client)};
 if(!process.argv.includes('--behavior-only')){step('browser captures and assertions');result.captures=await captures(client);step('repository strips');result.repositories=await repositories(client);}
 client.checkFatal();
 await client.close();
 if(!process.argv.includes('--behavior-only')){step('busy server');result.busy=await busy();}
 await writeFile(client.artifacts+'/checks.json',JSON.stringify(result,null,2)+'\n');
 console.log('UI browser assertions passed. Artifacts: '+client.artifacts);
} catch(error){console.error('ui-browser: FAIL ('+error.message+')');process.exitCode=1;}
finally{try{await client?.close();}catch(error){console.error('ui-browser: FAIL ('+error.message+')');process.exitCode=1;}}
