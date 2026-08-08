/* ---------- flight manual: expanders with periapsis orbits ---------- */
var ORB='<svg class="orb" viewBox="0 0 64 40" aria-hidden="true">'
 +'<ellipse class="orbit" cx="32" cy="20" rx="26" ry="14"/>'
 +'<line class="peri" x1="59.5" y1="17" x2="59.5" y2="23"/>'
 +'<circle class="planet" cx="54" cy="20" r="3.4"/>'
 +'<circle class="sat" r="2" style="animation-delay:__D__"/>'
 +'<circle class="flare" cx="58" cy="20" r="3.2" style="animation-delay:__D__"/>'
 +'</svg>';
var FEATURES=[
 {t:'Git ingress & clone-through cache',one:'SSH in, smart protocol only. Repos fetched from your upstream on demand, cached, stale-served through outages.',
  b:'SSH on a fixed NodePort, <code>git-upload-pack</code>/<code>git-receive-pack</code> and nothing else — no shells, no PTYs, no forwarding. Repositories are discovered dynamically from the configured upstream: an agent clones through Oberth, Oberth fetches and caches. If the forge is down, the cached clone is served with a stale warning instead of failing the loop. Force-pushed feature branches are accepted — agents rebase constantly. Tags are creation-only: deletion, movement and collisions are rejected at the door.',
  c:['<b>NodePort</b> 30022','<b>tags</b> creation-only','<b>cache</b> periodic git gc','<b>outage</b> stale-serve, not fail']},
 {t:'Uplink identity',one:'One key fingerprint = one identity@hostname = one token. Every action attributed, nothing guessed.',
  b:'An uplink is the whole identity system: SSH key fingerprint → <code>identity@hostname</code> → bearer token. Tokens are stored only as SHA-256 digests — the database never holds a usable credential. Attribution is connection-bound: a push belongs to the uplink authenticated on that SSH connection, never to a guessed session. Agents sharing one key intentionally share one identity; finer attribution requires a distinct transport credential.',
  c:['<b>tokens</b> sha-256 at rest','<b>attribution</b> connection-bound','<b>shared key</b> = shared identity, by design']},
 {t:'One Job per run',one:'Each push spawns exactly one Kubernetes Job. FIFO queue, supersession, hard limits, TTL cleanup.',
  b:'Every push spawns one <code>batch/v1</code> Job in the oberth namespace — single container, no init-chains, no step controllers. Runs queue FIFO with a concurrency knob (default 3 on a single node). A newer push to the same branch <b>supersedes</b> the in-flight run, so agents never queue behind their own dead commits. Jobs carry resource requests and limits, an active deadline, and <code>ttlSecondsAfterFinished</code> — one unbounded job can\u2019t evict the CI system that spawned it.',
  c:['<b>queue</b> FIFO · default 3','<b>supersede</b> new push cancels stale run','<b>identity</b> no SA token · non-root 65534','<b>rootfs</b> read-only']},
 {t:'Minimal runner, repo-owned tools',one:'The runner image carries no toolchains. Your repository installs its own — pinned and checksummed.',
  b:'The runner is digest-pinned and deliberately empty: no Go, no Python, no linters, no scanners, no Helm. Your repository\u2019s setup steps install exactly the tools it needs — versions and SHA-256 checksums pinned in the repo — into a job-local directory that leads every step\u2019s merged <code>PATH</code>. The runner never resolves commands from its own ambient PATH, so a fat predecessor image can\u2019t silently supply an undeclared dependency. Changing a tool is a normal reviewable repository change, not a rebuild of a shared image.',
  c:['<b>image</b> digest-pinned, minimal','<b>7 cache slots</b> Go · Python · Zig · lint · Trivy · Helm · cosign','<b>archives</b> reverified before every use']},
 {t:'periapsis.go',one:'Your pipeline is a Go file with real exit codes and named logs. Run it on your laptop first.',
  b:'<code>.oberth/periapsis.go</code> is what workflow YAML always wanted to be: named burns and steps, dependencies, per-step exit codes and log slices — in a language your agents already write, executable locally before pushing. yaegi interprets it <b>inside the Job pod</b>, never in the server process: a buggy pipeline crashes its own Job, not Oberth. Steps run as subprocesses with their own process groups; a timed-out step is killed and joined whole, so nothing leaks across step boundaries.',
  c:['<b>timeout</b> 30 min/step default','<b>job deadline</b> 12 h','<b>logs</b> ≤64 MiB · per-step byte ranges']},
 {t:'Issues as work queue',one:'Red CI files exactly one issue per repo+branch. Green auto-closes it. Locks stop agent pile-ups.',
  b:'This is the breakup point where work vectors get lost in an agent swarm — an agent reaped, re-assigned, out of context. Red CI creates or updates exactly <b>one</b> open issue per repository+branch: repeat reds update it (no spam that kills the monitor-agent pattern), green auto-closes it. Issues carry the SHA, the failed step, a bounded failure tail and the command hint for the full log. Five-minute locks owned by the uplink let one agent claim an issue; the lock renews while they keep working it.',
  c:['<b>dedupe</b> 1 open issue / repo+branch','<b>lock</b> 5-min TTL · uplink-owned','<b>scope</b> sqlite = CI queue · forge = tracker of record']},
 {t:'Promote',one:'Green-gate the exact SHA, prove the merged tree, push non-forced. Append-only, no retries in place.',
  b:'Promotion is one definition, applied once: verify CI is really green for the exact SHA, fetch the target, do a forge-style local merge, run CI on the <b>merged tree</b>, and push non-forced. If the target moved meanwhile, the non-fast-forward rejection is the free concurrency guard — that promotion is terminal, fixes go to the feature branch, new SHA, new promote. If the merge is a fast-forward, the tested tree is already exactly that tree: no wasteful re-run.',
  c:['<b>records</b> append-only','<b>push</b> never forced','<b>fast-forward</b> skips the re-run']},
 {t:'Release burns',one:'Tags release only from promoted history. Secrets: declared ∩ allowlisted, snapshotted, masked.',
  b:'A tag gets a release burn only if its commit is reachable from the default branch — one <code>merge-base --is-ancestor</code> check, so nobody releases an arbitrary un-promoted SHA by slapping a tag on it. The mounted secrets are the exact intersection of the repository\u2019s literal <code>ReleaseSecrets</code> declaration (parsed statically, never executed) and the administrator allowlist — snapshotted into an immutable Job-owned Secret and registered with the log redactor before a byte hits disk. Branch pushes get nothing. Always.',
  c:['<b>gate</b> ancestry from default branch','<b>mount</b> ReleaseSecrets ∩ allowlist','<b>logs</b> streaming redaction']},
 {t:'Audit chain, anchored outside',one:'SHA-256-chained history, timestamped and witnessed where the box itself can\u2019t rewrite it.',
  b:'Audit rows form a gap-free SHA-256 chain with the acting uplink on every mutation. Chain heads receive RFC 3161 timestamps from an independent TSA and are published as Rekor witnesses under a stable identity. Before each publication, an immutable Kubernetes intent record is created that Oberth can create, get and list — but never update, patch or delete. Rolling back the entire PVC with a stale-but-valid public prefix therefore <b>fails closed</b>: the system detects the rewritten history instead of forgetting it.',
  c:['<b>TSA</b> Sectigo · RFC 3161 over HTTPS','<b>witness</b> Rekor, pinned by UUID','<b>rollback</b> detected, fail-closed']},
 {t:'Secret store release secrets',one:'Declared in one literal map, fetched from your OpenBao or Vault at release admission, delivered to build memory only — never etcd.',
  b:'Point Oberth at your OpenBao or HashiCorp Vault and release pipelines get their credentials the way they should: fetched at release admission with the server’s own Kubernetes ServiceAccount identity — validated by TokenReview, with no code path that accepts a store admin token. Values travel over the Kubernetes exec stream into a memory-backed volume inside the running release Job — <code>$OBERTH_SECRETSTORE_DIR/&lt;name&gt;/&lt;key&gt;</code>, <code>0400</code>, tmpfs — never a Kubernetes Secret, never etcd, never a node disk, and every value is masked in the live log stream. Before the Job exists, Oberth logs in and reads every declared path: a missing secret, an unreachable store, or a path outside the administrator allowlist fails the release immediately, with the exact path in the error. Setup is one drift-guarded script — <code>curl -fsSL https://oberth.ci/setup-secretstore.sh | bash</code> — run next to your store; <code>oberth secretstore verify</code> proves the whole trust chain from inside the pod, no credentials needed.',
  c:['<b>store</b> OpenBao · HashiCorp Vault','<b>tokens</b> read-only · 10-min TTL · revoked after fetch','<b>delivery</b> exec stream → tmpfs · masked logs','<b>branch builds</b> zero credentials']}
];
var PAGEMAP=['setup','trust','pipeline','pipeline','pipeline','agents','agents','trust','trust','trust'];
function layoutReturn(){
  var flow=document.getElementById('sflow');if(!flow)return;
  var agent=document.getElementById('agentNode'),issue=document.getElementById('issueChip');
  if(!agent||!issue)return;
  flow.querySelectorAll('.sret,.sretv').forEach(function(e){e.remove()});
  var fr=flow.getBoundingClientRect(),ar=agent.getBoundingClientRect(),ir=issue.getBoundingClientRect();
  if(ir.width===0)return;
  var ax=ar.left-fr.left+ar.width/2, ix=ir.left-fr.left+ir.width/2;
  var yTop1=ir.bottom-fr.top+4, yTop2=ar.bottom-fr.top+4;
  var yLine=Math.max(yTop1,yTop2)+16;
  var h=document.createElement('div');h.className='sret';
  h.style.left=ax+'px';h.style.width=(ix-ax)+'px';h.style.top=yLine+'px';
  h.innerHTML='<span class="lbl">agents lock &amp; fix → new push</span><i></i>';
  flow.appendChild(h);
  var v1=document.createElement('div');v1.className='sretv';
  v1.style.left=ix+'px';v1.style.top=yTop1+'px';v1.style.height=(yLine-yTop1)+'px';
  flow.appendChild(v1);
  var v2=document.createElement('div');v2.className='sretv up';
  v2.style.left=ax+'px';v2.style.top=yTop2+'px';v2.style.height=(yLine-yTop2)+'px';
  flow.appendChild(v2);
}
addEventListener('resize',layoutReturn);
setTimeout(layoutReturn,60);
document.fonts&&document.fonts.ready.then(function(){setTimeout(layoutReturn,50)});
document.querySelectorAll('.schip').forEach(function(b){
  b.addEventListener('click',function(){location.hash='#/'+PAGEMAP[+b.dataset.f]});
});
/* full system write-ups live on their subject pages */
(function(){
  var byPage={};
  FEATURES.forEach(function(f,i){var pg=PAGEMAP[i];(byPage[pg]=byPage[pg]||[]).push(f)});
  Object.keys(byPage).forEach(function(pg){
    var main=document.getElementById('pg-'+pg);if(!main)return;
    var sec=document.createElement('section');sec.style.paddingTop='0';
    var html='<div class="wrap"><div class="secno rv in"><b>'+pg.toUpperCase()+' /</b> Systems in depth</div>';
    byPage[pg].forEach(function(f){
      html+='<div class="depth-item"><h3>'+f.t+'</h3><span class="one">'+f.one+'</span><p>'+f.b+'</p>'
        +'<div class="exp-chips">'+f.c.map(function(c){return '<span class="exp-chip">'+c+'</span>'}).join('')+'</div></div>';
    });
    html+='</div>';
    sec.innerHTML=html;
    main.insertBefore(sec,main.querySelector('.cta-mini'));
  });
})();

/* ---------- oberth effect two-body sim ---------- */
(function(){
  var cv=document.getElementById('obCanvas');if(!cv)return;
  var ctx=cv.getContext('2d');
  var status=document.getElementById('obStatus');
  var reduce=matchMedia('(prefers-reduced-motion: reduce)').matches;
  var TS=2.6;
  var W=0,H=0,DPR=1,cx=0,cy=0,GM=0,rp=0,ra=0,hubX=0,hubY=0,dockGlow=0;
  var PLANET_R=60;
  var stars=[],galaxies=[];
  function size(){
    DPR=Math.min(devicePixelRatio||1,2);
    W=cv.clientWidth;H=cv.clientHeight;
    if(!W||!H)return;
    cv.width=W*DPR;cv.height=H*DPR;
    ctx.setTransform(DPR,0,0,DPR,0,0);
    cx=W*0.5;cy=H*0.30;                     /* planet upper-center */
    rp=PLANET_R+42;                          /* periapsis right below the planet */
    var raWant=H*1.7;                        /* apoapsis far off-screen top */
    var bCap=cx-56;                          /* keep the left swing in frame */
    ra=Math.min(raWant,(bCap*bCap)/rp);
    GM=30000*(H/300);
    hubX=W-92;hubY=Math.max(64,H*0.30);          /* the upstream hub, on the escape path */
    galaxies=[];
    for(var gi=0;gi<4;gi++){
      galaxies.push({x:(0.08+Math.random()*0.84)*W,y:(0.08+Math.random()*0.84)*H,
        rot:Math.random()*3.14,rx:14+Math.random()*22,ry:4+Math.random()*7,
        a:0.05+Math.random()*0.05,edge:gi===1});
    }
    stars=[];
    for(var i=0;i<190;i++)stars.push({x:Math.random()*W,y:Math.random()*H,a:0.10+Math.random()*0.14,r:0.7+Math.random()*0.5,tw:Math.random()*6.28,ts:0.4+Math.random()*1.2});  /* distant dust */
    for(var j=0;j<80;j++)stars.push({x:Math.random()*W,y:Math.random()*H,a:0.22+Math.random()*0.22,r:1.1+Math.random()*1.0,tw:Math.random()*6.28,ts:0.2+Math.random()*0.6});
    for(var k2=0;k2<9;k2++)stars.push({x:Math.random()*W,y:Math.random()*H,a:0.55+Math.random()*0.3,r:1.6+Math.random()*0.7,tw:Math.random()*6.28,ts:0.5+Math.random()*0.8,glint:1});
  }
  size();addEventListener('resize',size);
  window.obSimResize=size;

  function orbitSpeed(r,a){return Math.sqrt(Math.max(0,GM*(2/r-1/a)))}
  var parts=[],pulses=[],crashFx=[];
  var lastLaunch=-99,STAGGER=9.6,simT=0;
  var STEP_NAMES=['checkout','lint','test','build','package'];
  function mkShip(mode,delay){
    return {mode:mode,delay:delay,phase:'wait',x:0,y:0,vx:0,vy:0,trail:[],
            burnLeft:0,burnDvPF:0,prevR:1e9,approaching:false,holdT:0,msg:'standing by',
            steps:[0,0,0,0,0],failIdx:2,r0:0,ang:0};
  }
  function launch(sh){
    var a=(rp+ra)/2;
    sh.x=cx;sh.y=cy-ra;sh.vx=-orbitSpeed(ra,a);sh.vy=0;   /* apoapsis, entering the left descent */
    /* warp forward silently until the ship is just above the frame */
    var guard=0;
    while(sh.y<-70 && guard++<24000){
      var dx=cx-sh.x,dy=cy-sh.y,r2=dx*dx+dy*dy,r=Math.sqrt(r2),ag=GM/r2,h=1/60;
      sh.vx+=ag*dx/r*h;sh.vy+=ag*dy/r*h;sh.x+=sh.vx*h;sh.y+=sh.vy*h;
    }
    sh.trail=[];sh.phase='coast';sh.burnLeft=0;sh.prevR=1e9;sh.approaching=false;sh.holdT=0;
    sh.steps=[0,0,0,0,0];
    sh.failIdx=3;                                   /* red run always dies at 'build', at periapsis */
    sh.r0=Math.hypot(sh.x-cx,sh.y-cy);
    sh.msg='inbound from high orbit · coasting to periapsis';
    lastLaunch=simT;
  }
  var ships=[mkShip('pro',0.3),mkShip('retro',9.9)];

  function setStatus(){
    var A=ships[0],B=ships[1];
    status.innerHTML='<span class="sg">◆</span> '+A.msg+' &nbsp;<span class="c-dimmer">·</span>&nbsp; <span class="sr">◆</span> '+B.msg;
  }

  function stepShip(sh,dt){
    if(sh.phase==='wait'){ if(simT>=sh.delay && simT-lastLaunch>=STAGGER)launch(sh); return; }
    if(sh.phase==='crashed'||sh.phase==='gone'){
      sh.holdT+=dt;
      if(sh.holdT>1.0 && simT-lastLaunch>=STAGGER)launch(sh);
      return;
    }
    if(sh.phase==='docked'){
      sh.holdT+=dt;dockGlow=1;
      if(sh.trail.length)sh.trail.shift();          /* trail retracts gracefully */
      if(sh.holdT>2.4 && simT-lastLaunch>=STAGGER)launch(sh);
      return;
    }
    if(sh.phase==='docking'){                        /* eased final approach — no jerk */
      var tx=hubX-19,ty=hubY,k=Math.min(1,dt*3.6);
      sh.x+=(tx-sh.x)*k;sh.y+=(ty-sh.y)*k;
      var da=((0-sh.ang+Math.PI)%6.2832+6.2832)%6.2832-Math.PI;
      sh.ang+=da*Math.min(1,dt*4.2);
      if(sh.trail.length)sh.trail.shift();
      if(Math.hypot(tx-sh.x,ty-sh.y)<0.8){
        sh.x=tx;sh.y=ty;sh.ang=0;sh.phase='docked';sh.holdT=0;
        sh.msg='<b class="sg">docked at upstream</b> · synced ✓ · next pass queued';
        setStatus();
      }
      return;
    }
    var sub=4,h=dt/sub;
    for(var s2=0;s2<sub;s2++){
      var dx=cx-sh.x,dy=cy-sh.y,r2=dx*dx+dy*dy,r=Math.sqrt(r2);
      var ag=GM/r2;
      sh.vx+=ag*dx/r*h;sh.vy+=ag*dy/r*h;
      if(sh.phase==='escape'){                     /* arrival guidance toward the hub */
        var gx=hubX-16-sh.x,gy=hubY-sh.y,gd=Math.hypot(gx,gy)||1;
        var vmax=Math.min(190,12+gd*1.55);
        var vdx=gx/gd*vmax,vdy=gy/gd*vmax;
        var axm=2.7;
        sh.vx+=(vdx-sh.vx)*axm*h;sh.vy+=(vdy-sh.vy)*axm*h;
      }
      if(sh.burnLeft>0){
        var sp=Math.hypot(sh.vx,sh.vy)||1,ux=sh.vx/sp,uy=sh.vy/sp;
        var dir=(sh.mode==='pro')?1:-1;
        sh.vx+=dir*sh.burnDvPF*ux*h;sh.vy+=dir*sh.burnDvPF*uy*h;
        sh.burnLeft-=h;
        var emit=(sh.mode==='pro')?2:(Math.random()<0.7?2:0);   /* retro engine sputters */
        for(var p=0;p<emit;p++){
          parts.push({x:sh.x,y:sh.y,
            vx:-dir*ux*95+(Math.random()-.5)*40,
            vy:-dir*uy*95+(Math.random()-.5)*40,
            life:0.4+Math.random()*0.25,age:0,
            col:sh.mode==='pro'?'74,222,128':'255,107,107'});
        }
        if(sh.mode==='retro'&&Math.random()<0.5){                /* misfire sparkles */
          for(var sk=0;sk<3;sk++){var sa=Math.random()*6.283,ss=30+Math.random()*90;
            parts.push({x:sh.x,y:sh.y,vx:Math.cos(sa)*ss,vy:Math.sin(sa)*ss,
              life:0.2+Math.random()*0.3,age:0,
              col:Math.random()<0.5?'245,215,110':'255,150,90'});}
        }
        if(sh.burnLeft<=0){
          if(sh.mode==='pro'){sh.phase='escape';sh.msg='<b class="sg">escape → GREEN</b> · branch synced ↑'}
          else{sh.phase='decay';sh.msg='<b class="sr">orbit collapsing → RED</b>'}
          setStatus();
        }
      }
      if(sh.phase==='coast'){
        if(r<sh.prevR){sh.approaching=true}
        else if(sh.approaching && r<rp*1.35){
          sh.phase='burn';
          var v=Math.hypot(sh.vx,sh.vy),dur=0.6;
          if(sh.mode==='pro'){
            var vesc=Math.sqrt(2*GM/r);
            sh.burnDvPF=Math.max(0,(1.18*vesc-v))/dur;
            for(var si=0;si<5;si++)sh.steps[si]=1;   /* CI finished — all green at periapsis */
            sh.msg='<b class="sy">PERIAPSIS</b> · CI green · prograde burn <span class="sg">Δv+'+(((1.18*vesc-v))|0)+'</span>';
          }else{
            var vt=0.45*Math.sqrt(GM/r);
            sh.burnDvPF=Math.max(0,(v-vt))/dur;
            for(var sj=0;sj<sh.failIdx;sj++)sh.steps[sj]=1;
            sh.steps[sh.failIdx]=2;                  /* misfire = step goes red */
            sh.msg='<b class="sy">PERIAPSIS</b> · <span class="sr">'+STEP_NAMES[sh.failIdx]+' ✗</span> · retrograde burn <span class="sr">Δv−'+(((v-vt))|0)+'</span>';
          }
          sh.burnLeft=dur;sh.approaching=false;
          pulses.push({x:sh.x,y:sh.y,age:0,col:sh.mode==='pro'?'74,222,128':'255,107,107'});
          setStatus();
        }
        sh.prevR=r;
      }
      sh.x+=sh.vx*h;sh.y+=sh.vy*h;
    }
    sh.ang=Math.atan2(sh.vy,sh.vx);
    if(sh.phase==='coast'){                       /* CI steps tick over on the way in */
      var rNow=Math.hypot(sh.x-cx,sh.y-cy);
      var prog=Math.max(0,Math.min(1,(sh.r0-rNow)/Math.max(1,sh.r0-rp*1.15)));
      var cap=(sh.mode==='pro')?4:sh.failIdx;      /* green holds 'package' for periapsis; red never finishes its doomed step */
      var n=Math.min(cap,Math.floor(prog*5.2));
      for(var st2=0;st2<n;st2++)if(sh.steps[st2]===0)sh.steps[st2]=1;
    }
    if(sh.phase==='decay'&&Math.random()<0.6){                   /* damaged, trailing sparks */
      var da=Math.random()*6.283,ds=15+Math.random()*55;
      parts.push({x:sh.x,y:sh.y,vx:Math.cos(da)*ds,vy:Math.sin(da)*ds,
        life:0.25+Math.random()*0.35,age:0,
        col:Math.random()<0.5?'245,215,110':'255,130,100'});
    }
    var col=(sh.phase==='escape')?'#4ade80':(sh.phase==='decay')?'#ff6b6b':(sh.phase==='burn'?(sh.mode==='pro'?'#a6e8bd':'#ffb3b3'):'#8a93ab');
    sh.trail.push({x:sh.x,y:sh.y,c:col});
    if(sh.trail.length>220)sh.trail.shift();
    var rr=Math.hypot(sh.x-cx,sh.y-cy);
    if(sh.phase==='decay' && rr<PLANET_R+4){
      sh.phase='crashed';sh.holdT=0;
      for(var b=0;b<46;b++){var an=Math.random()*6.283,sp3=40+Math.random()*130;
        parts.push({x:sh.x,y:sh.y,vx:Math.cos(an)*sp3,vy:Math.sin(an)*sp3,life:0.4+Math.random()*0.7,age:0,col:'255,107,107'})}
      crashFx.push({x:sh.x,y:sh.y,age:0});
      sh.msg='<b class="sr">✗ impact · RED</b> · issue <span class="sp">#47</span> filed → new sha, new orbit';
      setStatus();
    }
    if(sh.phase==='escape'){
      var hd=Math.hypot(hubX-19-sh.x,hubY-sh.y);
      if(hd<34&&Math.hypot(sh.vx,sh.vy)<120){      /* begin eased final approach */
        sh.phase='docking';sh.vx=0;sh.vy=0;
        sh.msg='final approach · docking at <span class="sp">upstream</span>';
        setStatus();
      }else if(sh.x>W+140||sh.y<-160||sh.x<-140||sh.y>H+140){
        sh.phase='gone';sh.holdT=0;sh.msg='<b class="sg">GREEN</b> delivered';setStatus();
      }
    }
  }

  function step(dt){
    simT+=dt;
    dockGlow=Math.max(0,dockGlow-dt*0.7);
    for(var pu=pulses.length-1;pu>=0;pu--){pulses[pu].age+=dt;if(pulses[pu].age>0.9)pulses.splice(pu,1)}
    for(var cf=crashFx.length-1;cf>=0;cf--){crashFx[cf].age+=dt;if(crashFx[cf].age>4.2)crashFx.splice(cf,1)}
    ships.forEach(function(sh){stepShip(sh,dt)});
    for(var i=parts.length-1;i>=0;i--){
      var q=parts[i];q.age+=dt;
      if(q.age>q.life){parts.splice(i,1);continue}
      q.x+=q.vx*dt;q.y+=q.vy*dt;
    }
  }

  function drawPlanet(){
    /* jupiter, line-drawing style — subtle */
    ctx.save();
    ctx.beginPath();ctx.arc(cx,cy,PLANET_R,0,6.283);ctx.clip();
    var g=ctx.createRadialGradient(cx-16,cy-18,10,cx,cy,PLANET_R);
    g.addColorStop(0,'#6b5c49');g.addColorStop(0.55,'#4c4238');g.addColorStop(1,'#2c2a33');
    ctx.fillStyle=g;ctx.fillRect(cx-PLANET_R,cy-PLANET_R,PLANET_R*2,PLANET_R*2);
    ctx.fillStyle='rgba(201,160,108,0.06)';
    ctx.fillRect(cx-PLANET_R,cy-27,PLANET_R*2,13);
    ctx.fillStyle='rgba(176,111,74,0.075)';
    ctx.fillRect(cx-PLANET_R,cy+13,PLANET_R*2,14);
    var bands=[
      {dy:-46,c:'201,160,108',a:0.20,w:1},
      {dy:-34,c:'185,128,90', a:0.34,w:1.6},
      {dy:-20,c:'214,178,128',a:0.22,w:1},
      {dy: -7,c:'176,111,74', a:0.40,w:2},
      {dy:  7,c:'201,160,108',a:0.24,w:1},
      {dy: 20,c:'185,128,90', a:0.36,w:1.8},
      {dy: 34,c:'214,178,128',a:0.20,w:1},
      {dy: 46,c:'176,111,74', a:0.26,w:1.2}
    ];
    bands.forEach(function(bd){
      var chord=Math.sqrt(Math.max(0,PLANET_R*PLANET_R-bd.dy*bd.dy));
      if(chord<6)return;
      ctx.strokeStyle='rgba('+bd.c+','+bd.a+')';ctx.lineWidth=bd.w;
      ctx.beginPath();ctx.ellipse(cx,cy+bd.dy,chord,Math.max(2.5,chord*0.13),0,0,6.283);ctx.stroke();
    });
    /* great red spot */
    ctx.strokeStyle='rgba(217,119,87,0.55)';ctx.lineWidth=1.4;
    ctx.fillStyle='rgba(217,119,87,0.22)';
    ctx.beginPath();ctx.ellipse(cx+PLANET_R*0.34,cy+PLANET_R*0.30,11,6,-0.15,0,6.283);
    ctx.fill();ctx.stroke();
    /* limb shading */
    var shd=ctx.createRadialGradient(cx-14,cy-16,PLANET_R*0.3,cx,cy,PLANET_R);
    shd.addColorStop(0,'rgba(0,0,0,0)');shd.addColorStop(0.75,'rgba(16,17,26,0.05)');shd.addColorStop(1,'rgba(16,17,26,0.55)');
    ctx.fillStyle=shd;ctx.fillRect(cx-PLANET_R,cy-PLANET_R,PLANET_R*2,PLANET_R*2);
    ctx.restore();
    /* ring */
    ctx.strokeStyle='rgba(196,176,247,0.30)';ctx.lineWidth=1.2;
    ctx.beginPath();ctx.arc(cx,cy,PLANET_R+8,0,6.283);ctx.stroke();
  }

  function draw(){
    ctx.clearRect(0,0,W,H);
    var tnow=performance.now()/1000;
    /* distant galaxies */
    for(var gg=0;gg<galaxies.length;gg++){
      var gx2=galaxies[gg];
      ctx.save();ctx.translate(gx2.x,gx2.y);ctx.rotate(gx2.rot);
      var gr=ctx.createRadialGradient(0,0,0,0,0,gx2.rx);
      gr.addColorStop(0,'rgba(236,238,246,'+(gx2.a*1.6)+')');
      gr.addColorStop(0.45,'rgba(196,176,247,'+(gx2.a)+')');
      gr.addColorStop(1,'rgba(196,176,247,0)');
      ctx.fillStyle=gr;
      ctx.beginPath();ctx.ellipse(0,0,gx2.rx,gx2.edge?gx2.ry*0.45:gx2.ry,0,0,6.283);ctx.fill();
      if(!gx2.edge){                                /* faint spiral hint */
        ctx.strokeStyle='rgba(236,238,246,'+(gx2.a*0.9)+')';ctx.lineWidth=0.8;
        ctx.beginPath();ctx.arc(2,0,gx2.rx*0.5,0.4,2.6);ctx.stroke();
        ctx.beginPath();ctx.arc(-2,0,gx2.rx*0.5,3.5,5.8);ctx.stroke();
      }
      ctx.restore();
    }
    for(var i=0;i<stars.length;i++){var st=stars[i];
      var al2=st.a*(0.72+0.28*Math.sin(tnow*st.ts+st.tw));
      ctx.globalAlpha=al2;
      ctx.fillStyle='#eceef6';ctx.fillRect(st.x,st.y,st.r,st.r);
      if(st.glint){                                   /* tiny cross glint on the bright ones */
        ctx.globalAlpha=al2*0.55;ctx.strokeStyle='#eceef6';ctx.lineWidth=0.7;
        ctx.beginPath();ctx.moveTo(st.x+st.r/2-4,st.y+st.r/2);ctx.lineTo(st.x+st.r/2+4,st.y+st.r/2);
        ctx.moveTo(st.x+st.r/2,st.y+st.r/2-4);ctx.lineTo(st.x+st.r/2,st.y+st.r/2+4);ctx.stroke();
      }}
    ctx.globalAlpha=1;
    /* reference ellipse — vertical major axis, apoapsis far above the frame */
    var a=(rp+ra)/2,b=Math.sqrt(rp*ra),ecy=cy-(a-rp);
    ctx.save();ctx.translate(cx,ecy);
    ctx.strokeStyle='rgba(255,255,255,0.09)';ctx.setLineDash([3,5]);ctx.lineWidth=1;
    ctx.beginPath();ctx.ellipse(0,0,b,a,0,0,6.283);ctx.stroke();ctx.setLineDash([]);
    ctx.restore();
    /* periapsis marker — right below the planet */
    ctx.strokeStyle='rgba(236,238,246,0.55)';ctx.lineWidth=1.2;
    ctx.beginPath();ctx.moveTo(cx-10,cy+rp);ctx.lineTo(cx+10,cy+rp);ctx.stroke();
    ctx.fillStyle='rgba(236,238,246,0.8)';ctx.font='600 12px JetBrains Mono, monospace';
    ctx.fillText('periapsis',cx+16,cy+rp+4);
    /* upstream hub — line drawing */
    (function(){
      var tt=performance.now()/1000;
      var anyDocked=ships.some(function(q){return q.phase==='docked'});
      ctx.save();ctx.translate(hubX,hubY);
      /* rotating outer ring with spokes */
      ctx.strokeStyle='rgba(196,176,247,0.55)';ctx.lineWidth=1.2;
      ctx.beginPath();ctx.arc(0,0,15,0,6.283);ctx.stroke();
      ctx.strokeStyle='rgba(196,176,247,0.22)';ctx.lineWidth=1;
      var rot=tt*0.35;
      for(var sp2=0;sp2<6;sp2++){
        var aa=rot+sp2*1.047;
        ctx.beginPath();ctx.moveTo(Math.cos(aa)*6,Math.sin(aa)*6);
        ctx.lineTo(Math.cos(aa)*15,Math.sin(aa)*15);ctx.stroke();
      }
      /* core — solid hub */
      ctx.fillStyle='rgba(196,176,247,0.16)';
      ctx.beginPath();ctx.arc(0,0,6,0,6.283);ctx.fill();
      ctx.strokeStyle='rgba(196,176,247,0.6)';ctx.lineWidth=1.2;
      ctx.beginPath();ctx.arc(0,0,6,0,6.283);ctx.stroke();
      ctx.fillStyle='rgba(196,176,247,0.55)';
      ctx.beginPath();ctx.arc(0,0,1.8,0,6.283);ctx.fill();
      /* docking arm + port */
      ctx.strokeStyle='rgba(196,176,247,0.55)';
      ctx.beginPath();ctx.moveTo(-15,0);ctx.lineTo(-27,0);ctx.stroke();
      ctx.fillStyle='rgba(196,176,247,0.32)';                    /* solid dock block */
      ctx.fillRect(-29,-3.2,3.4,6.4);
      ctx.beginPath();ctx.moveTo(-27,-4.5);ctx.lineTo(-27,4.5);ctx.stroke();
      if(anyDocked){                                             /* payload flowing into upstream */
        var pt=(tt*1.15)%1;
        ctx.fillStyle='rgba(74,222,128,'+(0.85*Math.sin(pt*3.14))+')';
        ctx.beginPath();ctx.arc(-27+22*pt,0,1.7,0,6.283);ctx.fill();
      }
      /* port guide lights — green when occupied, slow white otherwise */
      var pl=anyDocked?(0.5+0.5*Math.sin(tt*6)):(0.25+0.25*Math.sin(tt*2));
      ctx.fillStyle=anyDocked?'rgba(74,222,128,'+pl+')':'rgba(236,238,246,'+pl+')';
      ctx.beginPath();ctx.arc(-27,-6.5,1.2,0,6.283);ctx.fill();
      ctx.beginPath();ctx.arc(-27,6.5,1.2,0,6.283);ctx.fill();
      /* solar booms + angled panel pairs with grid */
      ctx.strokeStyle='rgba(196,176,247,0.4)';
      ctx.beginPath();ctx.moveTo(0,-15);ctx.lineTo(0,-20);ctx.moveTo(0,15);ctx.lineTo(0,20);ctx.stroke();
      [[-1,-20],[1,20]].forEach(function(w){
        ctx.save();ctx.translate(0,w[1]);ctx.rotate(w[0]*0.12);
        ctx.fillStyle='rgba(125,211,252,0.10)';                  /* glass panels */
        ctx.fillRect(-7,w[0]<0?-14:0,14,14);
        ctx.strokeRect(-7,w[0]<0?-14:0,14,14);
        for(var gl=1;gl<4;gl++){
          var gy2=(w[0]<0?-14:0)+gl*3.5;
          ctx.beginPath();ctx.moveTo(-7,gy2);ctx.lineTo(7,gy2);ctx.stroke();
        }
        ctx.beginPath();ctx.moveTo(0,w[0]<0?-14:0);ctx.lineTo(0,w[0]<0?0:14);ctx.stroke();
        ctx.restore();
      });
      /* comms dish */
      ctx.strokeStyle='rgba(196,176,247,0.5)';
      ctx.beginPath();ctx.moveTo(11,-10);ctx.lineTo(17,-16);ctx.stroke();
      ctx.fillStyle='rgba(196,176,247,0.12)';
      ctx.beginPath();ctx.arc(19.5,-18.5,4.2,2.1,4.6);ctx.fill();
      ctx.beginPath();ctx.arc(19.5,-18.5,4.2,2.1,4.6);ctx.stroke();
      ctx.beginPath();ctx.arc(19.5,-18.5,0.9,0,6.283);ctx.stroke();
      /* beacon on the ring */
      var bx=Math.cos(rot*1.3)*15,by=Math.sin(rot*1.3)*15;
      ctx.fillStyle='rgba(255,107,107,'+(0.3+0.5*(0.5+0.5*Math.sin(tt*3.2)))+')';
      ctx.beginPath();ctx.arc(bx,by,1.3,0,6.283);ctx.fill();
      ctx.restore();
      ctx.font='600 9px JetBrains Mono, monospace';
      ctx.fillStyle='rgba(236,238,246,0.55)';
      ctx.fillText('upstream',hubX-21,hubY+48);
      if(dockGlow>0){
        ctx.fillStyle='rgba(74,222,128,'+Math.min(1,dockGlow*1.4)+')';
        ctx.fillText('\u2713 synced',hubX-21,hubY+60);
      }
    })();
    drawPlanet();
    ships.forEach(function(sh){
      for(var t=1;t<sh.trail.length;t++){
        var p0=sh.trail[t-1],p1=sh.trail[t];
        ctx.globalAlpha=t/sh.trail.length*0.95;
        ctx.strokeStyle=p1.c;ctx.lineWidth=2.2;
        ctx.beginPath();ctx.moveTo(p0.x,p0.y);ctx.lineTo(p1.x,p1.y);ctx.stroke();
      }
      ctx.globalAlpha=1;
    });
    for(var pq=0;pq<pulses.length;pq++){
      var pz=pulses[pq],pk=pz.age/0.9;
      ctx.strokeStyle='rgba('+pz.col+','+(0.5*(1-pk))+')';ctx.lineWidth=1.4;
      ctx.beginPath();ctx.arc(pz.x,pz.y,6+pk*26,0,6.283);ctx.stroke();
    }
    for(var cq=0;cq<crashFx.length;cq++){
      var cz=crashFx[cq];
      ctx.fillStyle='rgba(20,16,16,'+(0.4*(1-cz.age/4.2))+')';
      ctx.beginPath();ctx.arc(cz.x,cz.y,6,0,6.283);ctx.fill();
      if(cz.age<2.6){
        ctx.font='600 10px JetBrains Mono, monospace';
        ctx.fillStyle='rgba(255,107,107,'+(0.95*(1-cz.age/2.6))+')';
        ctx.fillText('issue #47',cz.x+10,cz.y-8-cz.age*9);
      }
    }
    for(var q=0;q<parts.length;q++){
      var pa=parts[q],al=1-pa.age/pa.life;
      ctx.fillStyle='rgba('+pa.col+','+(al*0.9)+')';
      ctx.beginPath();ctx.arc(pa.x,pa.y,2.4,0,6.283);ctx.fill();
    }
    ships.forEach(function(sh){
      if(sh.phase==='wait'||sh.phase==='crashed'||sh.phase==='gone')return;
      var docked=(sh.phase==='docked'||sh.phase==='docking');
      var sp=Math.hypot(sh.vx,sh.vy)||1;
      var ux=docked?Math.cos(sh.ang):sh.vx/sp,uy=docked?Math.sin(sh.ang):sh.vy/sp;
      var pro=sh.mode==='pro';
      var hull=(sh.phase==='escape')?'#4ade80':(sh.phase==='decay')?'#ff6b6b':(pro?'#e9fbef':'#fde9e9');
      var nav=pro?'#4ade80':'#ff6b6b';
      ctx.save();ctx.translate(sh.x,sh.y);ctx.rotate(Math.atan2(uy,ux));
      if(sh.phase==='escape'||sh.phase==='decay'){ctx.shadowColor=hull;ctx.shadowBlur=18}
      ctx.fillStyle=hull;
      if(pro){ /* long dart */
        ctx.beginPath();ctx.moveTo(24,0);ctx.lineTo(-15,11.2);ctx.lineTo(-8.4,0);ctx.lineTo(-15,-11.2);ctx.closePath();ctx.fill();
      }else{  /* stub with twin fins */
        ctx.beginPath();ctx.moveTo(18,0);ctx.lineTo(-11,8.8);ctx.lineTo(-11,-8.8);ctx.closePath();ctx.fill();
        ctx.fillRect(-17,6.4,9.2,4.2);ctx.fillRect(-17,-10.6,9.2,4.2);
      }
      ctx.shadowBlur=0;
      ctx.fillStyle=nav;
      ctx.globalAlpha=0.55+0.45*Math.sin(performance.now()/140+(pro?0:2));
      ctx.beginPath();ctx.arc(pro?3:1,0,4.2,0,6.283);ctx.fill();
      ctx.globalAlpha=1;
      ctx.restore();
      if(docked)return;                            /* no step list at the pier */
      if(sh.y<10||sh.x<10||sh.x>W-10||sh.y>H-10)return;  /* and none before the ship enters the frame */
      /* tiny CI step list, following the ship */
      var lh=12.5,pad=5.5,bw=76,bh=5*lh+pad*2-2;
      var lx=sh.x+24,ly=sh.y-bh/2;
      if(lx+bw>W-4)lx=sh.x-24-bw;
      if(ly<4)ly=4; if(ly+bh>H-4)ly=H-4-bh;
      ctx.fillStyle='rgba(13,15,22,0.62)';
      ctx.beginPath();
      if(ctx.roundRect)ctx.roundRect(lx,ly,bw,bh,3);else ctx.rect(lx,ly,bw,bh);
      ctx.fill();
      ctx.font='600 10px JetBrains Mono, monospace';
      for(var li=0;li<5;li++){
        var stt=sh.steps[li];
        var cc=stt===1?'rgba(74,222,128,0.95)':stt===2?'rgba(255,107,107,0.98)':'rgba(236,238,246,0.35)';
        var sym=stt===1?'✓':stt===2?'✗':'·';
        ctx.fillStyle=cc;
        ctx.fillText(sym+' '+STEP_NAMES[li],lx+pad,ly+pad+9+li*lh);
      }
    });
  }

  var last=0;
  function loop(ts){
    if(!last)last=ts;
    var dt=Math.min(0.033,(ts-last)/1000);last=ts;
    if(W&&H){ if(!reduce)step(dt*TS); draw(); }
    requestAnimationFrame(loop);
  }
  setStatus();
  if(reduce&&W&&H){
    for(var k=0;k<260;k++)step(1/60*TS);
    status.innerHTML='animation paused (reduced motion) · <span class="sg">prograde = green escape</span> · <span class="sr">retrograde = red decay</span>';
  }
  requestAnimationFrame(loop);
  cv.addEventListener('click',function(){for(var k=0;k<110;k++)step(1/50*TS)});
})();

/* ---------- reveal on scroll ---------- */
var io=new IntersectionObserver(function(es){es.forEach(function(e){if(e.isIntersecting){e.target.classList.add('in');io.unobserve(e.target)}})},{threshold:.1});
document.querySelectorAll('.rv:not(.in)').forEach(function(el){io.observe(el)});

/* ---------- progress + scrollspy ---------- */
addEventListener('scroll',function(){
  var h=document.documentElement;
  document.getElementById('prog').style.width=(h.scrollTop/(h.scrollHeight-h.clientHeight)*100)+'%';
},{passive:true});
var ROUTES=['home','pipeline','agents','trust','setup','alpha','try'];
var TITLES={
  home:'Oberth — local CI at agent speed',
  pipeline:'Oberth — periapsis.go pipeline',
  agents:'Oberth — thirteen MCP tools for AI agents',
  trust:'Oberth — the trigger is the security boundary',
  setup:'Oberth — deploy and use',
  alpha:'Oberth — alpha pilot ledger',
  try:'Oberth — quickstart: five steps to a gated push'
};
function route(){
  var h=(location.hash||'#/').replace('#/','')||'home';
  if(ROUTES.indexOf(h)<0)h='home';
  if(TITLES[h])document.title=TITLES[h];
  document.querySelectorAll('.page').forEach(function(p){p.classList.toggle('on',p.id==='pg-'+h)});
  document.querySelectorAll('#nl a[data-r]').forEach(function(a){a.classList.toggle('on',a.dataset.r===h)});
  scrollTo(0,0);
  if(h==='home'&&window.obSimResize)setTimeout(window.obSimResize,30);
  if(h==='home'&&typeof layoutReturn==='function')setTimeout(layoutReturn,60);
  document.querySelectorAll('#pg-'+h+' .rv:not(.in)').forEach(function(el){io.observe(el)});
  if(h==='try')setTimeout(bindK8sTabs,0);
}
addEventListener('hashchange',route);
route();
var howBtn=document.getElementById('howBtn');
if(howBtn)howBtn.addEventListener('click',function(){
  var el=document.getElementById('how-it-works-sec');
  if(el)el.scrollIntoView({behavior:'smooth'});
});

/* ---------- hero control loop replay ---------- */
var HERO=[
 {h:'<span class="pr">$</span> git push oberth feat/rotate-keys'},
 {h:'<span class="rm">remote:</span> [oberth] uplink <span class="c-pur">agent-07@runner</span>'},
 {h:'<span class="rm">remote:</span> ref accepted · run queued',s:'QUEUED',c:'qd'},
 {h:'<span class="rm">remote:</span> job <span class="c-cyn">run-8f3a2c</span> · periapsis.go',s:'RUNNING',c:'run'},
 {h:'<span class="rm">remote:</span> setup ✓ &nbsp;lint ✓ &nbsp;test ✓ &nbsp;build ✓',s:'GREEN',c:'grn'},
 {h:'<span class="rm">remote:</span> branch synced → upstream, exact tested sha',s:'SYNCED',c:'syn'},
 {h:'<span class="rm">remote:</span> issue #47 (repo+branch) auto-closed',s:'CLOSED',c:'dim'}
];
var heroLog=document.getElementById('heroLog'),heroTimer=[];
function clearTimers(a){a.forEach(clearTimeout);a.length=0}
function playHero(){
  clearTimers(heroTimer);heroLog.innerHTML='';
  var t=0;
  HERO.forEach(function(l,i){
    t+= i===0? 200 : (i===1?700:620);
    heroTimer.push(setTimeout(function(){
      var d=document.createElement('div');d.className='tl';
      d.innerHTML=l.h+(l.s?'<span class="st '+l.c+'">'+l.s+'</span>':'');
      heroLog.appendChild(d);
      requestAnimationFrame(function(){requestAnimationFrame(function(){d.classList.add('show')})});
    },t));
  });
  heroTimer.push(setTimeout(function(){
    var d=document.createElement('div');d.className='tl show';
    d.innerHTML='<span class="pr">$</span> <span class="caret"></span>';
    heroLog.appendChild(d);
  },t+700));
}
document.getElementById('replayBtn').addEventListener('click',playHero);
playHero();

/* ---------- control-loop stepper ---------- */
var STEPS=[
 {n:'receive · git ingress',d:'<b>Authenticate the uplink, attribute the actor, resolve the repo.</b> Only the Git smart protocol is accepted — no shells, no forwarding. Unknown repo? Oberth fetches it from your upstream and caches it. Forge down? The cached clone is served with a stale warning instead of failing.',
  t:'<div class="tl show"><span class="pr">$</span> git push oberth feat/rotate-keys</div><div class="tl show"><span class="rm">remote:</span> uplink <span class="c-pur">agent-07@runner</span> · repo cached ✓</div><div class="tl show"><span class="rm">remote:</span> force-update on feature branch <span class="st grn ml0">ACCEPTED</span></div><div class="tl show"><span class="rm">remote:</span> tag delete/move <span class="st red ml0">REJECTED</span> <span class="cm">· tags are creation-only</span></div>'},
 {n:'run · one kubernetes job',d:'<b>One push, one Job, one container.</b> The digest-pinned minimal runner interprets your <b>periapsis.go</b> with yaegi; steps run as subprocesses with their own exit codes and named logs. FIFO queue, default 3 concurrent; a newer push to the same branch supersedes the stale run.',
  t:'<div class="tl show"><span class="rm">job:</span> run-8f3a2c <span class="cm">· automountServiceAccountToken: false</span></div><div class="tl show"><span class="rm">step:</span> toolchain <span class="cm">— pinned go1.24.5, sha256 ✓</span><span class="st grn">4s</span></div><div class="tl show"><span class="rm">step:</span> lint<span class="st grn">18s</span></div><div class="tl show"><span class="rm">step:</span> test -race<span class="st run">RUNNING</span></div><div class="tl show"><span class="cm"># timeout 30m/step · job deadline 12h · logs ≤64 MiB</span></div>'},
 {n:'record · red or green',d:'<b>Red creates exactly one issue per repo+branch</b> — repeat reds update it, so an agent iterating on a failure can\u2019t spam the queue. Green force-syncs the exact tested branch to your forge and auto-closes the issue. Green work always exists upstream — no local-only limbo.',
  t:'<div class="tl show"><span class="rm">run:</span> red · failed step <span class="c-red">build_arm64</span></div><div class="tl show"><span class="rm">issue:</span> #47 updated <span class="cm">(not duplicated — same repo+branch)</span></div><div class="tl show"><span class="rm">issue:</span> locked for agent-07 <span class="cm">· TTL 5m, renews on touch</span></div><div class="tl show mt-4"><span class="rm">later:</span> green → branch synced ✓ · issue #47 <span class="st grn">AUTO-CLOSED</span></div>'},
 {n:'promote · merged-tree proof',d:'<b>Promotion is one definition:</b> green-gate the exact SHA, merge with the target locally, run CI on the merged tree, push non-forced. If the target moved, the non-fast-forward rejection is the free concurrency guard — the promote goes red and stays red; fixes ship as a new SHA, new promote.',
  t:'<div class="tl show"><span class="pr">mcp:</span> promote 9c1d44 main <span class="st c-pur">pr-2841</span></div><div class="tl show"><span class="rm">oberth:</span> green-gate ✓ · local merge · CI on merged tree</div><div class="tl show"><span class="rm">oberth:</span> push --no-force → main<span class="st syn">SYNCED</span></div><div class="tl show"><span class="cm"># fast-forward? already tested exactly that tree — no re-run</span></div>'},
 {n:'release · tag burn',d:'<b>Tag the promoted commit and push the tag.</b> Oberth checks it\u2019s reachable from the default branch, runs the release burn with exactly the declared∩allowlisted secrets, tests against the real published artifacts — and only then syncs the tag upstream, where your own release pipelines pick it up.',
  t:'<div class="tl show"><span class="pr">$</span> git push oberth v0.11.0</div><div class="tl show"><span class="rm">gate:</span> merge-base --is-ancestor main ✓</div><div class="tl show"><span class="rm">burn:</span> release · secrets: <span class="c-yel">r2-publisher, cosign-key</span> <span class="cm">· masked</span></div><div class="tl show"><span class="rm">verify:</span> published artifacts re-downloaded, checksums ✓<span class="st grn">GREEN</span></div><div class="tl show"><span class="rm">tag:</span> synced → upstream<span class="st syn">COMPLETE</span></div>'}
];
var stepDesc=document.getElementById('stepDesc'),stepTerm=document.getElementById('stepTerm'),stepTitle=document.getElementById('stepTermTitle');
function setStep(i){
  document.querySelectorAll('.step-tab').forEach(function(b){b.classList.toggle('on',+b.dataset.i===i)});
  stepDesc.innerHTML=STEPS[i].d;
  stepTitle.innerHTML='<b>'+STEPS[i].n+'</b>';
  stepTerm.innerHTML=STEPS[i].t;
}
document.querySelectorAll('.step-tab').forEach(function(b){b.addEventListener('click',function(){setStep(+b.dataset.i)})});
setStep(0);

/* ---------- mcp session replay ---------- */
var MCP=[
 {h:'<span class="pr">→</span> status feat/rotate-keys'},
 {h:'<span class="rm">←</span> red · failed: <span class="c-red">build_arm64</span> · issue #47 · lock acquired <span class="cm">5m</span>'},
 {h:'<span class="pr">→</span> logs 8f3a2c build_arm64'},
 {h:'<span class="rm">←</span> 41 lines <span class="cm">· only that step ·</span> "ld: cannot find -lssl"'},
 {h:'<span class="cm">   …agent fixes, pushes. superseded run cancelled, new run queued.</span>'},
 {h:'<span class="pr">→</span> wait 9c1d44'},
 {h:'<span class="rm">←</span> <span class="c-grn">green</span> · 4m 02s · issue #47 auto-closed'},
 {h:'<span class="pr">→</span> promote 9c1d44 main'},
 {h:'<span class="rm">←</span> promote id <span class="c-pur">pr-2841</span> · merged-tree CI running'},
 {h:'<span class="pr">→</span> promote_status pr-2841'},
 {h:'<span class="rm">←</span> <span class="c-grn">green</span> · main updated, non-forced · append-only record'}
];
var mcpLog=document.getElementById('mcpLog'),mcpTimer=[];
function playMcp(){
  clearTimers(mcpTimer);mcpLog.innerHTML='';
  var t=0;
  MCP.forEach(function(l){
    t+=560;
    mcpTimer.push(setTimeout(function(){
      var d=document.createElement('div');d.className='tl';d.innerHTML=l.h;
      mcpLog.appendChild(d);
      requestAnimationFrame(function(){requestAnimationFrame(function(){d.classList.add('show')})});
    },t));
  });
}
document.getElementById('mcpReplay').addEventListener('click',playMcp);
var mcpIO=new IntersectionObserver(function(es){es.forEach(function(e){if(e.isIntersecting){playMcp();mcpIO.disconnect()}})},{threshold:.3});
mcpIO.observe(document.getElementById('mcpLog'));

/* ---------- config tabs ---------- */
var CFG={
 claude:{t:'.claude/settings.json',c:'{\n  <span class="c-s">"mcpServers"</span>: {\n    <span class="c-s">"oberth"</span>: {\n      <span class="c-s">"type"</span>: <span class="c-s">"url"</span>,\n      <span class="c-s">"url"</span>: <span class="c-s">"https://oberth.host:30443/mcp"</span>,\n      <span class="c-s">"headers"</span>: {\n        <span class="c-s">"Authorization"</span>: <span class="c-s">"Bearer oberth_…"</span>\n      }\n    }\n  }\n}'},
 codex:{t:'config.toml',c:'<span class="c-cm"># via the standard stdio→HTTP bridge</span>\n[mcp_servers.oberth]\ncommand = <span class="c-s">"npx"</span>\nargs = [<span class="c-s">"-y"</span>, <span class="c-s">"mcp-remote"</span>,\n        <span class="c-s">"https://oberth.host:30443/mcp"</span>,\n        <span class="c-s">"--header"</span>,\n        <span class="c-s">"Authorization: Bearer oberth_…"</span>]'}
};
document.querySelectorAll('.tab2').forEach(function(b){
  b.addEventListener('click',function(){
    document.querySelectorAll('.tab2').forEach(function(x){x.classList.remove('on')});
    b.classList.add('on');
    document.getElementById('cfgTitle').textContent=CFG[b.dataset.cfg].t;
    document.getElementById('cfgCode').innerHTML=CFG[b.dataset.cfg].c;
  });
});

/* ---------- copy buttons ---------- */
document.querySelectorAll('.cp').forEach(function(b){
  b.addEventListener('click',function(){
    var pre=b.closest('.code').querySelector('pre');
    if(navigator.clipboard)navigator.clipboard.writeText(pre.textContent);
    var o=b.textContent;b.textContent='COPIED ✓';setTimeout(function(){b.textContent=o},1300);
  });
});

/* ---------- quickstart engine tabs ---------- */
var K8S={
 k3s:{t:'k3s <em>single binary, systemd-managed</em>',c:'<span class="c-cm"># Install k3s — lightweight, production-grade</span>\ncurl -sfL https://get.k3s.io | sh -s - --disable=traefik \\\n  --write-kubeconfig-mode 644'},
 kind:{t:'kind <em>runs in Docker, ephemeral</em>',c:'<span class="c-cm"># Install kind — clusters inside Docker containers</span>\nkind create cluster --name oberth'},
 k0s:{t:'k0s <em>single binary, controller + worker</em>',c:'<span class="c-cm"># Install k0s — zero-friction Kubernetes</span>\ncurl -sSfL https://get.k0s.sh | sudo sh\nsudo k0s install controller --single --enable-worker\nsudo k0s start'}
};
function bindK8sTabs(){
  var tabs=document.getElementById('k8sTabs');if(!tabs)return;
  if(tabs.dataset.bound)return;
  tabs.dataset.bound='1';
  tabs.querySelectorAll('.tab2').forEach(function(b){
    b.addEventListener('click',function(){
      tabs.querySelectorAll('.tab2').forEach(function(x){x.classList.remove('on')});
      b.classList.add('on');
      var k=b.dataset.k8s;
      document.getElementById('k8sTitle').innerHTML=K8S[k].t;
      document.getElementById('k8sCode').innerHTML=K8S[k].c;
    });
  });
}
bindK8sTabs();
