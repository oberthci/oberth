#!/usr/bin/env python3
"""Unexecuted CPU2/offline public watch fixture build, bound to immutable source.

No fixture daemon, certificate, bearer, private RAM or production context is
created. Requires a separately supplied root coordinator handoff and permit.
"""
from pathlib import Path
import argparse
import ctypes
import hashlib
import json
import os
import resource
import shutil
import signal
import subprocess
import tarfile
import time
from watch_csa_admission import check,public_json
from watch_csa_common import source_guard

SOURCE=Path(__file__).resolve().parent
REPO=SOURCE.parent


def refuse():
    raise RuntimeError('source-bound fixture helper build failed')


def digest(path):
    with Path(path).open('rb') as file:
        return hashlib.file_digest(file,'sha256').hexdigest()




def finish(child):
    # Default SIGCHLD/sole-thread ownership makes the unreaped direct leader
    # safe to signal. Once reaped, never signal its old PGID: join only actual
    # adopted direct children through fresh pidfds and verified PPid.
    residual=False
    if child is not None and child.poll() is None:
        try:
            fd=os.pidfd_open(child.pid)
            try:
                signal.pidfd_send_signal(fd,signal.SIGKILL)
            finally:
                os.close(fd)
        except ProcessLookupError:
            pass
        child.wait(timeout=10)
    deadline=time.monotonic()+10
    while True:
        pids=[int(value) for value in Path(f'/proc/self/task/{os.getpid()}/children').read_text().split()]
        if not pids:
            return residual
        for pid in pids:
            try:
                fd=os.pidfd_open(pid)
                try:
                    parent=next(line.split()[1] for line in Path(f'/proc/{pid}/status').read_text().splitlines() if line.startswith('PPid:'))
                    if int(parent)!=os.getpid():
                        refuse()
                    signal.pidfd_send_signal(fd,signal.SIGKILL); residual=True
                finally:
                    os.close(fd)
                os.waitpid(pid,os.WNOHANG)
            except (FileNotFoundError,ProcessLookupError,ChildProcessError):
                pass
        if time.monotonic()>deadline:
            refuse()
        time.sleep(.01)


def main():
    parser=argparse.ArgumentParser()
    parser.add_argument('--output',type=Path,required=True)
    parser.add_argument('--reviewed-source-sha',required=True)
    parser.add_argument('--packet',type=Path,required=True)
    parser.add_argument('--admission',type=Path,required=True)
    args=parser.parse_args()
    if signal.getsignal(signal.SIGCHLD)!=signal.SIG_DFL or len(list(Path('/proc/self/task').iterdir()))!=1:
        refuse()
    if ctypes.CDLL(None).prctl(36,1,0,0,0)!=0:
        refuse()
    resource.setrlimit(resource.RLIMIT_CORE,(0,0))
    resource.setrlimit(resource.RLIMIT_FSIZE,(256<<20,256<<20))
    def interrupted(signum,frame):
        raise RuntimeError('fixture helper build interrupted')
    for sig in [signal.SIGTERM,signal.SIGHUP]:
        signal.signal(sig,interrupted)
    manifest,snapshot=source_guard(args.packet,args.reviewed_source_sha)
    for name in ['watch-csa-build.py','watch_csa_admission.py','watch_csa_common.py']:
        if digest(SOURCE/name)!=manifest['files']['hack/'+name]['sha256']:
            refuse()
    admission=check(args.admission,'watch-csa-helper-build',args.reviewed_source_sha)
    pins,pins_sha=public_json(SOURCE/'watch-csa-tools.json')
    if pins_sha!=manifest['files']['hack/watch-csa-tools.json']['sha256']:
        refuse()
    go=Path(pins['go']['path'])
    if digest(go)!=pins['go']['sha256']:
        refuse()
    git=Path(pins['git']['path'])
    if digest(git)!=pins['git']['sha256']:
        refuse()
    if not args.output.is_absolute() or args.output.parent.resolve(strict=True)!=args.output.parent:
        refuse()
    args.output.mkdir(mode=0o700,exist_ok=False)
    for name in ['home','tmp','cache','module']:
        (args.output/name).mkdir(mode=0o700)
    env={'PATH':str(go.parent)+':/usr/bin:/bin','HOME':str(args.output/'home'),
         'TMPDIR':str(args.output/'tmp'),'GOTMPDIR':str(args.output/'tmp'),'GOCACHE':str(args.output/'cache'),
         'GOMODCACHE':'/home/yellowmegaman/go/pkg/mod','GOMAXPROCS':'2','GOFLAGS':'-mod=readonly -p=2',
         'GOTOOLCHAIN':'local','GOENV':'off','GOWORK':'off','GOPROXY':'off','GOSUMDB':'off',
         'CGO_ENABLED':'0','GOOS':'linux','GOARCH':'amd64','LANG':'C','LC_ALL':'C'}
    receipt={'passed':False,'scope':'offline public watch fixture build only; no API fixture execution',
             'source_manifest_sha256':args.reviewed_source_sha,'fixture_source_packet_sha256':args.reviewed_source_sha,
             'crypto_source_sha256':digest(snapshot/'hack/watch-csa-crypto.go'),'runner_sha256':digest(Path(__file__)),
             'admission':admission,'go':pins['go'],'stages':[],'all_owned_children_joined':False}
    with (args.output/'ATTEMPT.json').open('x') as file:
        json.dump(receipt,file,indent=2); file.write('\n'); file.flush(); os.fsync(file.fileno())
    child=None
    def stage(name,argv,cwd,output):
        nonlocal child
        check(args.admission,'watch-csa-helper-build',args.reviewed_source_sha)
        started=time.monotonic(); row={'name':name,'command':argv,'exit':None,'all_owned_children_joined':False}
        receipt['stages'].append(row)
        log=args.output/(name+'.log')
        with log.open('xb') as errors,output.open('xb') as stdout:
            child=subprocess.Popen(argv,cwd=cwd,env=env,stdout=stdout,stderr=errors,start_new_session=True)
            row['pid']=child.pid
            try:
                row['exit']=child.wait(timeout=600)
            finally:
                residual=finish(child); row['all_owned_children_joined']=True
                row['residual_children_terminated']=residual; child=None
        row['seconds']=round(time.monotonic()-started,3); row['log_sha256']=digest(log)
        if row['exit']!=0 or row['residual_children_terminated']:
            refuse()
    failed=False
    try:
        archive=args.output/'base.tar'
        stage('archive',[str(git),'archive','--format=tar',manifest['base']],REPO,archive)
        if archive.stat().st_size>128<<20:
            refuse()
        with tarfile.open(archive) as bundle:
            bundle.extractall(args.output/'module',filter='data')
        for name,want in manifest['files'].items():
            target=args.output/'module'/name; target.parent.mkdir(parents=True,exist_ok=True)
            shutil.copyfile(snapshot/name,target)
            if digest(target)!=want['sha256']:
                refuse()
        helper_source=args.output/'module'/'hack'/'watch-csa-crypto'
        helper_source.mkdir(exist_ok=False)
        data=(snapshot/'hack/watch-csa-crypto.go').read_bytes()
        prefix=b'//go:build ignore\n\n'
        if not data.startswith(prefix):
            refuse()
        (helper_source/'main.go').write_bytes(data[len(prefix):])
        receipt['compiled_crypto_main_sha256']=digest(helper_source/'main.go')
        crypto=args.output/'watch-csa-crypto'
        stage('crypto-build',[str(go),'build','-trimpath','-buildvcs=false','-mod=readonly','-p=2','-o',str(crypto),'./hack/watch-csa-crypto'],args.output/'module',args.output/'crypto.stdout')
        test=args.output/'watch-csa-real.test'
        stage('test-build',[str(go),'test','-c','-tags=watch_real_api','-trimpath','-buildvcs=false','-mod=readonly','-p=2','-o',str(test),'./internal/installer'],args.output/'module',args.output/'test.stdout')
        source_guard(args.packet,args.reviewed_source_sha)
        receipt['crypto_sha256']=digest(crypto)
        receipt['test_sha256']=digest(test)
        receipt['tools_sha256']=pins_sha
        receipt['passed']=True
    except BaseException:
        failed=True
    finally:
        try:
            finish(child); receipt['all_owned_children_joined']=True
        except BaseException:
            failed=True
        receipt['passed']=receipt['passed'] and not failed
        with (args.output/'BUILD-PROOF.json').open('x') as file:
            json.dump(receipt,file,indent=2); file.write('\n'); file.flush(); os.fsync(file.fileno())
    return 0 if receipt['passed'] else 1


if __name__=='__main__':
    raise SystemExit(main())
