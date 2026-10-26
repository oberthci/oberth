#!/usr/bin/env python3
"""Deterministic bounded-tree Git mirror fixture (no network)."""
import argparse, pathlib, random, subprocess, time, json
parser=argparse.ArgumentParser()
parser.add_argument('destination')
parser.add_argument('--commits',type=int,default=60000)
parser.add_argument('--blob-bytes',type=int,default=20480)
args=parser.parse_args()
root=pathlib.Path(args.destination).resolve()
if root.exists(): raise SystemExit('destination must not exist')
subprocess.run(['git','init','--quiet','--bare',str(root)],check=True)
subprocess.run(['git','-C',str(root),'symbolic-ref','HEAD','refs/heads/main'],check=True)
rng=random.Random(1205)
started=time.monotonic()
log=open(str(root)+'.fast-import.log','wb')
process=subprocess.Popen(['git','-C',str(root),'fast-import','--quiet'],stdin=subprocess.PIPE,stderr=log)
out=process.stdin
objects=0
try:
    for i in range(args.commits):
        # 60k commits + 60k trees + 80k unique blobs = 200k objects.
        body=('fixture commit %d\n'%i).encode()
        header=('commit refs/heads/main\ncommitter Fixture <fixture@example.invalid> %d +0000\ndata %d\n'%(1600000000+i,len(body))).encode()
        out.write(header);out.write(body)
        for j in range(2 if i%3==0 else 1):
            # Rotate a fixed set of paths to keep every tree bounded.
            out.write(('M 100644 inline data-%02d.bin\ndata %d\n'%((2*i+j)%32,args.blob_bytes)).encode())
            out.write(rng.randbytes(args.blob_bytes));out.write(b'\n');objects+=1
        out.write(b'\n')
    out.write(b'done\n');out.close()
except BaseException:
    process.kill();process.wait();raise
if process.wait()!=0: raise SystemExit('git fast-import failed; see '+log.name)
log.close()
subprocess.run(['git','-C',str(root),'repack','-ad','--depth=1','--window=0'],check=True)
subprocess.run(['git','-C',str(root),'count-objects','-v'],check=True)
print(json.dumps({'commits':args.commits,'blob_objects':objects,'approx_total_objects':2*args.commits+objects,'blob_bytes':args.blob_bytes,'elapsed_s':time.monotonic()-started,'pack_bytes':sum(p.stat().st_size for p in root.glob('objects/pack/*.pack'))}))
