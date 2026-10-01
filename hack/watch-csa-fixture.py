#!/usr/bin/env python3
"""SOURCE ONLY. A fresh root CPU2 admission is required before any RAM/launch.

This runs only an owned synthetic API; it has no production context input.
The copied custody module is byte-identical to reviewed623v5. No623 source or
process is imported, changed, launched or used as a cleanup target.
"""
from pathlib import Path
import argparse
import base64
import hashlib
import json
import os
import re
import secrets
import shutil
import signal
import socket
import ssl
import stat
import time
import urllib.request
import urllib.error
from watch_csa_admission import check, public_json
from watch_csa_common import digest, source_guard
from watch_csa_custody import Owner, guard, ram_guard

SOURCE = Path(__file__).resolve().parent


def refuse():
    raise RuntimeError('owned watch fixture qualification failed')


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, request, file, code, message, headers, new_url):
        refuse()


def port():
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0))
        return sock.getsockname()[1]


def private_bytes(path):
    fd = os.open(path, os.O_RDONLY | os.O_CLOEXEC | os.O_NOFOLLOW)
    try:
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode) or info.st_size > 65536:
            refuse()
        raw = bytearray()
        while len(raw) <= 65536:
            part = os.read(fd, 65537-len(raw))
            if not part:
                break
            raw.extend(part)
        if len(raw) > 65536:
            refuse()
        return raw
    finally:
        os.close(fd)


def owned_write(reservation, name, raw):
    fd = os.open(reservation['home']/name, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    try:
        os.fchown(fd, reservation['uid'], reservation['uid'])
        view = memoryview(raw)
        while view:
            count = os.write(fd, view)
            if count <= 0:
                refuse()
            view = view[count:]
    finally:
        os.close(fd)


def copy_role(source, reservation, name):
    raw = private_bytes(source/name)
    try:
        owned_write(reservation, name, raw)
    finally:
        raw[:] = b'\0'*len(raw)


def await_exit(owner, child, timeout):
    deadline = time.monotonic()+timeout
    while owner.poll(child) is None:
        raw = owner.drain(child)
        raw[:] = b'\0'*len(raw)
        if time.monotonic() >= deadline:
            refuse()
        time.sleep(.01)
    owner.finish(child)
    if child['exit'] != 0:
        refuse()


def original_public_proofs(home, names):
    value, _ = public_json(home/'public-original-attempts.json',65536)
    if set(value)!={'original_attempts'} or not isinstance(value['original_attempts'],list):
        refuse()
    found=set()
    fields={'scenario','namespace','original_conflicts','confirmed_metadata_commits','failed_revision','failed_status','failed_record_uid','failed_record_resource_version'}
    expected={'ServiceAccount/cloudflared-watch.metadata.labels.app.kubernetes.io/name',
              'ConfigMap/cloudflared-watch-openbao-ca.metadata.labels.app.kubernetes.io/name',
              'ConfigMap/cloudflared-watch-oberth-origin-ca.metadata.labels.app.kubernetes.io/name',
              'Deployment/cloudflared-watch-oberth-v2.metadata.labels.app.kubernetes.io/name',
              'Deployment/cloudflared-watch-oberth-v2.metadata.labels.app.kubernetes.io/instance',
              'Deployment/cloudflared-watch-oberth-v2.spec.template.spec.initContainers[name="fetch-token"].args'}
    inventory={('ServiceAccount','cloudflared-watch'),('ConfigMap','cloudflared-watch-openbao-ca'),('ConfigMap','cloudflared-watch-oberth-origin-ca'),('Deployment','cloudflared-watch-oberth-v2')}
    for row in value['original_attempts']:
        if set(row)!=fields or row['scenario'] not in names or row['scenario'] in found or not re.fullmatch(r'oberth-csa-owned-[a-z0-9]{5}',row['namespace']) or row['failed_revision']!=72 or row['failed_status']!='failed' or len(row['original_conflicts'])!=6 or set(row['original_conflicts'])!=expected:
            refuse()
        found.add(row['scenario'])
        if not re.fullmatch(r'[a-f0-9-]{36}',row['failed_record_uid']) or not re.fullmatch(r'[1-9][0-9]*',row['failed_record_resource_version']):
            refuse()
        confirmed=row['confirmed_metadata_commits']
        if len(confirmed)!=4 or {(entry['kind'],entry['name']) for entry in confirmed}!=inventory:
            refuse()
        for entry in confirmed:
            if set(entry)!={'kind','name','uid','resource_version'} or not re.fullmatch(r'[a-f0-9-]{36}',entry['uid']) or not re.fullmatch(r'[1-9][0-9]*',entry['resource_version']):
                refuse()
    return value['original_attempts']


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--packet', type=Path, required=True)
    parser.add_argument('--reviewed-source-sha', required=True)
    parser.add_argument('--build-receipt', type=Path, required=True)
    parser.add_argument('--admission', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--tools-file', type=Path)
    args = parser.parse_args()
    guard()
    packet, snapshot = source_guard(args.packet, args.reviewed_source_sha)
    for name in ['watch-csa-fixture.py', 'watch_csa_custody.py', 'watch_csa_admission.py', 'watch_csa_common.py', 'watch-csa-tools.json']:
        if digest(SOURCE/name) != packet['files']['hack/'+name]['sha256']:
            refuse()
    build, build_sha = public_json(args.build_receipt, 131072)
    if build.get('passed') is not True or build.get('all_owned_children_joined') is not True or build.get('fixture_source_packet_sha256') != args.reviewed_source_sha or build.get('runner_sha256') != packet['files']['hack/watch-csa-build.py']['sha256']:
        refuse()
    admission = check(args.admission, 'watch-csa-real-api', args.reviewed_source_sha, build['test_sha256'], build_sha)
    pins_path=args.tools_file or SOURCE/'watch-csa-tools.json'
    pins, pins_sha = public_json(pins_path)
    if pins_sha != packet['files']['hack/'+pins_path.name]['sha256'] or pins['api_version'] not in ['v1.36.2','v1.36.3']:
        refuse()
    if not args.output.is_absolute() or args.output.parent.resolve(strict=True) != args.output.parent:
        refuse()
    for ancestor in [args.output.parent, *args.output.parent.parents]:
        if not ancestor.stat().st_mode & 0o001:
            refuse()  # Reserved credentials must traverse public tool parents.
    args.output.mkdir(mode=0o755, exist_ok=False)
    os.chmod(args.output,0o755)  # Custody guard deliberately sets umask077.
    binaries = args.output/'binaries'
    binaries.mkdir(mode=0o755)
    os.chmod(binaries,0o755)
    selected = {name: (Path(pins[name]['path']), pins[name]['sha256']) for name in ['helm','kubectl','apiserver','etcd']}
    selected.update({'crypto': (args.build_receipt.parent/'watch-csa-crypto',build['crypto_sha256']), 'test': (args.build_receipt.parent/'watch-csa-real.test',build['test_sha256'])})
    for name, (source, wanted) in selected.items():
        if digest(source) != wanted:
            refuse()
        shutil.copyfile(source,binaries/name)
        os.chmod(binaries/name,0o555)
        if digest(binaries/name) != wanted:
            refuse()
    receipt = {'passed':False,'scope':'owned qualified API/Helm4.2.3 effect-engine regression only; released signed/live admission remains owed','api_version':pins['api_version'],'tools_sha256':pins_sha,
               'packet_sha256':args.reviewed_source_sha,'build_receipt_sha256':build_sha,'admission':admission,
               'all_owned_children_joined':False,'private_ram_removed':False,'named_results':{},'engine_diagnostics':{},'conflict_predicates':{}}
    with (args.output/'ATTEMPT.json').open('x') as file:
        json.dump(receipt,file,indent=2);file.write('\n');file.flush();os.fsync(file.fileno())
    owner = None
    ram = None
    failed = False
    test_home = None
    names={'same-UID-normal-Helm','shared-owner','extra-conflict','all-four-RV','expired','failed-record-RV','post-handoff-stop'}
    def interrupted(signum, frame):
        raise RuntimeError('owned watch fixture interrupted')
    for sig in [signal.SIGTERM,signal.SIGINT,signal.SIGHUP]:
        signal.signal(sig,interrupted)
    try:
        check(args.admission, 'watch-csa-real-api', args.reviewed_source_sha, build['test_sha256'], build_sha)
        ram = Path('/dev/shm')/('oberth-watch-csa-'+secrets.token_hex(12))
        ram.mkdir(mode=0o711,exist_ok=False)
        os.chmod(ram,0o711)
        ram_guard(ram,0o711,0)
        owner = Owner(ram)
        crypto = owner.reserve('crypto')
        child = owner.launch(crypto,binaries/'crypto',build['crypto_sha256'],[])
        await_exit(owner,child,60)
        etcd_home, api_home, test_home = owner.reserve('etcd'), owner.reserve('apiserver'), owner.reserve('test')
        for name in ['etcd-client-ca.crt','etcd-server.crt','etcd-server.key','etcd-peer-ca.crt','etcd-peer.crt','etcd-peer.key']:
            copy_role(crypto['home'],etcd_home,name)
        for name in ['api-client-ca.crt','api-server.crt','api-server.key','etcd-client-ca.crt','api-etcd-client.crt','api-etcd-client.key','sa-signing.key','sa-signing.pub']:
            copy_role(crypto['home'],api_home,name)
        for name in ['api-serving-ca.crt','admin-client.crt','admin-client.key']:
            copy_role(crypto['home'],test_home,name)
        ep,pp,ap=port(),port(),port()
        if len({ep,pp,ap})!=3:
            refuse()
        eh,ah=etcd_home['home'],api_home['home']
        etcd=owner.launch(etcd_home,binaries/'etcd',pins['etcd']['sha256'],
            ['--data-dir',str(eh/'data'),'--listen-client-urls',f'https://127.0.0.1:{ep}','--advertise-client-urls',f'https://127.0.0.1:{ep}',
             '--listen-peer-urls',f'https://127.0.0.1:{pp}','--initial-advertise-peer-urls',f'https://127.0.0.1:{pp}','--initial-cluster',f'default=https://127.0.0.1:{pp}',
             '--cert-file',str(eh/'etcd-server.crt'),'--key-file',str(eh/'etcd-server.key'),'--trusted-ca-file',str(eh/'etcd-client-ca.crt'),'--client-cert-auth',
             '--peer-cert-file',str(eh/'etcd-peer.crt'),'--peer-key-file',str(eh/'etcd-peer.key'),'--peer-trusted-ca-file',str(eh/'etcd-peer-ca.crt'),'--peer-client-cert-auth','--tls-min-version','TLS1.3'])
        api=owner.launch(api_home,binaries/'apiserver',pins['apiserver']['sha256'],
            ['--bind-address=127.0.0.1',f'--secure-port={ap}','--advertise-address=127.0.0.1',f'--etcd-servers=https://127.0.0.1:{ep}',
             '--etcd-cafile='+str(ah/'etcd-client-ca.crt'),'--etcd-certfile='+str(ah/'api-etcd-client.crt'),'--etcd-keyfile='+str(ah/'api-etcd-client.key'),
             '--service-cluster-ip-range=10.233.0.0/24','--authorization-mode=RBAC','--anonymous-auth=false','--tls-min-version=VersionTLS13',
             '--client-ca-file='+str(ah/'api-client-ca.crt'),'--tls-cert-file='+str(ah/'api-server.crt'),'--tls-private-key-file='+str(ah/'api-server.key'),
             '--service-account-issuer=https://owned-watch-fixture.invalid','--api-audiences=https://owned-watch-fixture.invalid',
             '--service-account-signing-key-file='+str(ah/'sa-signing.key'),'--service-account-key-file='+str(ah/'sa-signing.pub')])
        context=ssl.create_default_context(cafile=str(crypto['home']/'api-serving-ca.crt'))
        context.minimum_version=ssl.TLSVersion.TLSv1_3
        context.load_cert_chain(str(crypto['home']/'admin-client.crt'),str(crypto['home']/'admin-client.key'))
        opener=urllib.request.build_opener(urllib.request.ProxyHandler({}),urllib.request.HTTPSHandler(context=context),NoRedirect())
        opener.handlers=[handler for handler in opener.handlers if not isinstance(handler,urllib.request.HTTPHandler)]
        endpoint=f'https://127.0.0.1:{ap}/readyz'
        deadline=time.monotonic()+45
        while True:
            for child in [etcd,api]:
                raw=owner.drain(child);raw[:]=b'\0'*len(raw)
                if owner.poll(child) is not None:
                    refuse()
            try:
                with opener.open(endpoint,timeout=.5) as response:
                    if response.geturl()!=endpoint or response.status!=200 or response.read(17)!=b'ok':
                        refuse()
                break
            except (OSError,urllib.error.URLError):
                if time.monotonic()>=deadline:
                    refuse()
                time.sleep(.05)
        data={}
        for name in ['api-serving-ca.crt','admin-client.crt','admin-client.key']:
            raw=private_bytes(test_home['home']/name)
            try:
                data[name]=base64.b64encode(raw).decode('ascii')
            finally:
                raw[:]=b'\0'*len(raw)
        config={'apiVersion':'v1','kind':'Config','current-context':'watch-csa-isolated',
                'clusters':[{'name':'owned','cluster':{'server':f'https://127.0.0.1:{ap}','certificate-authority-data':data['api-serving-ca.crt']}}],
                'users':[{'name':'owned','user':{'client-certificate-data':data['admin-client.crt'],'client-key-data':data['admin-client.key']}}],
                'contexts':[{'name':'watch-csa-isolated','context':{'cluster':'owned','user':'owned'}}]}
        raw=bytearray(json.dumps(config).encode())
        try:
            owned_write(test_home,'kubeconfig.json',raw)
        finally:
            raw[:]=b'\0'*len(raw);data.clear();config.clear()
        owned_write(test_home,'tools.json',json.dumps({'helm':str(binaries/'helm'),'helm_sha256':pins['helm']['sha256'],'kubectl':str(binaries/'kubectl'),'kubectl_sha256':pins['kubectl']['sha256'],'api_version':pins['api_version']}).encode())
        check(args.admission, 'watch-csa-real-api', args.reviewed_source_sha, build['test_sha256'], build_sha)
        test=owner.launch(test_home,binaries/'test',build['test_sha256'],['-test.run=^TestWatchRecoveryRealAPI$','-test.v=true','-test.timeout=14m',
                          '-watch-fixture-config='+str(test_home['home']/'kubeconfig.json'),'-watch-fixture-tools='+str(test_home['home']/'tools.json')])
        deadline=time.monotonic()+15*60
        pending=bytearray()
        while True:
            for child in [etcd,api]:
                raw=owner.drain(child);raw[:]=b'\0'*len(raw)
                if owner.poll(child) is not None:
                    refuse()
            pending.extend(owner.drain(test))
            if len(pending)>65536 or time.monotonic()>deadline:
                refuse()
            while b'\n' in pending:
                line,_,rest=pending.partition(b'\n');pending[:]=rest
                match=re.fullmatch(rb'\s*--- (PASS|FAIL): TestWatchRecoveryRealAPI/([A-Za-z0-9-]+) \([0-9.]+s\)',line)
                if match and match[2].decode() in names:
                    receipt['named_results'][match[2].decode()]=match[1]==b'PASS'
                diagnostic=re.fullmatch(rb'\s*watch_recovery_real_test\.go:[0-9]+: WATCH_ENGINE ([A-Za-z0-9-]+) code=([0-9]{1,3}) receipts=([01]) ready=(true|false)',line)
                if diagnostic and diagnostic[1].decode() in names and int(diagnostic[2]) in [*range(85),999]:
                    receipt['engine_diagnostics'][diagnostic[1].decode()]={'error_code':int(diagnostic[2]),'confirmed_receipts':int(diagnostic[3]),'ready':diagnostic[4]==b'true'}
                conflict=re.fullmatch(rb'\s*watch_recovery_real_test\.go:[0-9]+: WATCH_CONFLICT ([A-Za-z0-9-]+) predicates=([0-9]{1,4})',line)
                if conflict and conflict[1].decode() in names and 0<=int(conflict[2])<=8191:
                    receipt['conflict_predicates'][conflict[1].decode()]=int(conflict[2])
                line[:]=b'\0'*len(line)
            if owner.poll(test) is not None:
                break
            time.sleep(.01)
        pending[:]=b'\0'*len(pending)
        owner.finish(test)
        if test['exit']!=0 or set(receipt['named_results'])!=names or not all(receipt['named_results'].values()):
            refuse()
        expected_codes={'same-UID-normal-Helm':0,'shared-owner':56,'extra-conflict':56,'all-four-RV':81,'expired':5,'failed-record-RV':12,'post-handoff-stop':84}
        if set(receipt['engine_diagnostics'])!=names or set(receipt['conflict_predicates'])!=names:
            refuse()
        for name,code in expected_codes.items():
            expected={'error_code':code,'confirmed_receipts':int(name in ['same-UID-normal-Helm','post-handoff-stop']),'ready':name=='same-UID-normal-Helm'}
            if receipt['engine_diagnostics'][name]!=expected or receipt['conflict_predicates'][name]!=8191:
                refuse()
        receipt['passed']=True
    except BaseException:
        failed=True
    finally:
        if owner is not None:
            try:
                owner.close()
                receipt['all_owned_children_joined']=True
            except BaseException:
                failed=True
            receipt['children']=owner.receipt()
        projection_ok=True
        if test_home is not None and (test_home['home']/'public-original-attempts.json').exists():
            try:
                receipt['preserved_original_attempts']=original_public_proofs(test_home['home'],names)
                if receipt['passed'] and len(receipt['preserved_original_attempts'])!=len(names):
                    refuse()
            except BaseException:
                failed=True;projection_ok=False
        elif receipt['passed']:
            failed=True;projection_ok=False
        if ram is not None and receipt['all_owned_children_joined'] and projection_ok:
            try:
                shutil.rmtree(ram)
                receipt['private_ram_removed']=not ram.exists()
            except BaseException:
                failed=True
        receipt['passed']=receipt['passed'] and not failed and receipt['all_owned_children_joined'] and receipt['private_ram_removed']
        with (args.output/'RESULT.json').open('x') as file:
            json.dump(receipt,file,indent=2);file.write('\n');file.flush();os.fsync(file.fileno())
    return 0 if receipt['passed'] else 1


if __name__=='__main__':
    raise SystemExit(main())
