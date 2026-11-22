import {writeFile} from 'node:fs/promises';
import {check} from './cdp.mjs';

// The repositories page: each row shows its last runs as bars, oldest left, in
// the example's spelled order (p passed, f failed, r running, q queued, c
// cancelled); every bar links to its run with status, ref and time as its title;
// a repository without runs says so. One read serves every row, and each row
// stays one line from 1024px.
const spelled={'origin/team/api':'pfpppcfppppfrppppqfp','origin/team/web':'ppfpr','mirror/tools/cli':''};
const colour={p:'passed',f:'failed',r:'running',q:'running',c:'other'};

export async function repositories(client){
 const results=[];
 await client.evaluate(`localStorage.setItem('oberth.token',${JSON.stringify(client.token)})`);
 await client.cdp('Emulation.setEmulatedMedia',{features:[{name:'prefers-color-scheme',value:'light'},{name:'prefers-reduced-motion',value:'reduce'}]});
 for(const width of [1280,1024,760,360]){
  await client.cdp('Emulation.setDeviceMetricsOverride',{width,height:900,deviceScaleFactor:1,mobile:false});
  const mark=client.requests.length;
  await client.loaded('/repos','.repos a.row');
  const page=await client.evaluate(`({scroll:document.documentElement.scrollWidth,head:Array.from(document.querySelectorAll('.repos .head span')).map(node=>node.textContent),rows:Array.from(document.querySelectorAll('.repos a.row')).map(row=>({key:row.dataset.key,height:row.getBoundingClientRect().height,latest:row.children[2].textContent,branch:row.children[1].textContent,cells:Array.from(row.children).map(cell=>cell.getBoundingClientRect().height),bars:Array.from(row.querySelectorAll('.bar')).map(bar=>({colour:bar.classList[1],href:bar.getAttribute('href'),title:bar.title,label:bar.getAttribute('aria-label'),tab:bar.tabIndex,left:bar.getBoundingClientRect().left,width:bar.getBoundingClientRect().width,height:bar.getBoundingClientRect().height}))}))})`);
  const reads=client.requests.slice(mark).filter(request=>request.path.startsWith('/api/repos')||request.path.startsWith('/api/runs'));
  check(reads.length===1&&new URL(reads[0].url).searchParams.get('recent_runs')==='true','the page did not read every strip with the repository list in one request '+JSON.stringify(reads));
  check(page.scroll<=width&&JSON.stringify(page.head)==='["REPOSITORY","DEFAULT BRANCH","LATEST RUN","LAST 20","BRANCHES"]','repositories page overflowed or lost its head '+JSON.stringify(page));
  for(const row of page.rows){
   const letters=spelled[row.key];
   check(JSON.stringify(row.bars.map(bar=>bar.colour))===JSON.stringify([...letters].map(letter=>colour[letter])),'bars out of order or miscoloured '+JSON.stringify(row));
   check(row.bars.every((bar,index)=>bar.href==='/runs/'+(0x7700+index).toString(16).padStart(32,'0')&&bar.title===bar.label&&bar.title.split(' · ')[1]==='topic-'+index&&bar.tab===-1&&bar.width===5&&bar.height===16&&(!index||bar.left>row.bars[index-1].left)),'bar link, title or shape wrong '+JSON.stringify(row.bars));
   if(!letters)check(row.latest==='no runs'&&!row.bars.length,'repository without runs not marked '+JSON.stringify(row));
   // From 1024px each row is one line: no cell taller than the status pill's line. Narrower, a
   // long latest-run line wraps rather than being cut; the bars keep their one line.
   if(width>=1024)check(row.cells.every(height=>height<=24)&&row.height<=44,'repository row is not one line '+JSON.stringify({width,row}));
  }
  const api=page.rows.find(row=>row.key==='origin/team/api');
  // The example carries stored refs (refs/heads/...) and a latest run from an earlier day; the row
  // prints branch names as the run list does.
  check(api.latest==='failed  · w5/own-m3 · Nov 15 15:00'&&api.branch==='main','latest run or default branch not shown as a branch name '+JSON.stringify(api));
  check(api.bars[0].title==='passed · topic-0 · Nov 15 19:00'&&api.bars[19].title==='passed · topic-19 · 14:00'&&api.bars[5].title.startsWith('cancelled · ')&&api.bars[17].title.startsWith('queued · '),'bar titles wrong '+JSON.stringify(api.bars.map(bar=>bar.title)));
  const capture=await client.cdp('Page.captureScreenshot',{format:'png',captureBeyondViewport:true,clip:{x:0,y:0,width,height:Math.max(900,await client.evaluate('document.documentElement.scrollHeight')),scale:1}});
  await writeFile(`${client.artifacts}/repositories-strip-${width}-light.png`,Buffer.from(capture.data,'base64'));
  results.push({width,...page});
 }
 // A bar opens its own run, not the row's run list.
 await client.evaluate(`document.querySelector('.repos a.row[data-key="origin/team/web"] .bar:last-child').click()`);
 const path=await client.evaluate(`new Promise(resolve=>{const test=()=>location.pathname.startsWith('/runs/')&&resolve(location.pathname+location.search);test();setTimeout(()=>resolve(location.pathname+location.search),1000);})`);
 check(path==='/runs/'+(0x7704).toString(16).padStart(32,'0'),'bar did not open its run '+path);
 await client.cdp('Emulation.setDeviceMetricsOverride',{width:1280,height:900,deviceScaleFactor:1,mobile:false});
 return results;
}
