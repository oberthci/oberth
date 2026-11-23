import {writeFile} from 'node:fs/promises';
import {launch,step} from './cdp.mjs';
import {behavior} from './behavior.mjs';
import {parallel} from './parallel.mjs';
import {captures} from './captures.mjs';
import {runHeader} from './run-header.mjs';
import {busy} from './busy.mjs';
import {statusSettings} from './status-settings.mjs';
import {repositories} from './repositories.mjs';
import {selectedWorkflow} from './selected-workflow.mjs';
import {liveness} from './liveness.mjs';

let client;
try {
 client=await launch();
 step('browser behavior assertions');
 const result={liveness:await liveness(client)};
 if(!process.argv.includes('--liveness-only')){result.behavior=await behavior(client);result.selectedWorkflow=await selectedWorkflow(client);}
 if(!process.argv.includes('--behavior-only')&&!process.argv.includes('--liveness-only')){step('effective settings');result.settings=await statusSettings(client);step('compact run header');result.runHeader=await runHeader(client);step('parallel step groups');result.parallel=await parallel(client);step('browser captures and assertions');result.captures=await captures(client);step('repository strips');result.repositories=await repositories(client);}
 client.checkFatal();
 await client.close();
 if(!process.argv.includes('--behavior-only')&&!process.argv.includes('--liveness-only')){step('busy server');result.busy=await busy();}
 await writeFile(client.artifacts+'/checks.json',JSON.stringify(result,null,2)+'\n');
 console.log('UI browser assertions passed. Artifacts: '+client.artifacts);
} catch(error){console.error('ui-browser: FAIL ('+error.message+')');process.exitCode=1;}
finally{try{await client?.close();}catch(error){console.error('ui-browser: FAIL ('+error.message+')');process.exitCode=1;}}
