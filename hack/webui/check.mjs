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
import {startingRun} from './starting-run.mjs';
import {progress} from './progress.mjs';

let client;
try {
 client=await launch();
 step('browser behavior assertions');
 const only=process.argv.find(arg=>arg.startsWith('--only='))?.slice(7);
 const result={};
 if(!only||only==='starting-run'){result.startingRun=[];for(const path of ['prepare',''])result.startingRun.push(await startingRun(client,path));}
 if(!only||only==='progress')result.progress=await progress();
 if(!only||only==='liveness')result.liveness=await liveness(client);
 if(!only)result.behavior=await behavior(client);
 if(!only){step('selected workflow views');result.selectedWorkflow=await selectedWorkflow(client);}
 if(!process.argv.includes('--behavior-only')&&!only){step('effective settings');result.settings=await statusSettings(client);step('compact run header');result.runHeader=await runHeader(client);step('parallel step groups');result.parallel=await parallel(client);step('browser captures and assertions');result.captures=await captures(client);step('repository strips');result.repositories=await repositories(client);}
 client.checkFatal();
 await client.close();
 if(!process.argv.includes('--behavior-only')&&!only){step('busy server');result.busy=await busy();}
 await writeFile(client.artifacts+'/checks.json',JSON.stringify(result,null,2)+'\n');
 console.log('UI browser assertions passed. Artifacts: '+client.artifacts);
} catch(error){console.error('ui-browser: FAIL ('+error.message+')');process.exitCode=1;}
finally{try{await client?.close();}catch(error){console.error('ui-browser: FAIL ('+error.message+')');process.exitCode=1;}}
