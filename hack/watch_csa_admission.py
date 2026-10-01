"""Public coordinator/CPU2 handoff guards. Importing launches nothing."""
from pathlib import Path
from datetime import datetime, timezone, timedelta
import hashlib
import json
import math
import os
import re
import stat



def refuse():
    raise RuntimeError('explicit fixture admission verification failed')


def public_json(path,limit=32768):
    path=Path(path)
    if not path.is_absolute() or path.resolve(strict=True)!=path:
        refuse()
    # One descriptor binds bounded input, strict parsing and its hash. Reject
    # changed/replaced files rather than combining bytes from separate reads.
    fd=os.open(path,os.O_RDONLY|os.O_CLOEXEC|os.O_NOFOLLOW)
    try:
        before=os.fstat(fd)
        if not stat.S_ISREG(before.st_mode) or before.st_size>limit:
            refuse()
        data=bytearray()
        while len(data)<=limit:
            chunk=os.read(fd,limit+1-len(data))
            if not chunk:
                break
            data.extend(chunk)
        if len(data)>limit:
            refuse()
        after=os.fstat(fd)
        named=path.lstat()
        binding=lambda info:(info.st_dev,info.st_ino,info.st_size,info.st_mtime_ns,info.st_ctime_ns)
        if binding(before)!=binding(after) or binding(after)!=binding(named) or len(data)!=after.st_size:
            refuse()
    finally:
        os.close(fd)
    def unique(pairs):
        value={}
        for key,item in pairs:
            if key in value:
                refuse()
            value[key]=item
        return value
    def finite_number(raw):
        if len(raw)>128:
            refuse()
        value=float(raw)
        if not math.isfinite(value):
            refuse()  # Includes finite-looking JSON such as 1e9999.
        return value
    def bounded_integer(raw):
        if len(raw)>128:
            refuse()
        return int(raw)
    def invalid_constant(raw):
        refuse()  # Python's otherwise accepted NaN/Infinity extension.
    value=json.loads(data,object_pairs_hook=unique,parse_float=finite_number,
                     parse_int=bounded_integer,parse_constant=invalid_constant)
    if not isinstance(value,dict):
        refuse()
    return value,hashlib.sha256(data).hexdigest()


def check(path,purpose,packet_sha,helper_sha=None,build_sha=None):
    value,digest=public_json(path)
    expected={'schema','approved','coordinator','purpose','lane','cpu','source_packet_sha256',
              'helper_sha256','build_receipt_sha256','issued_at','expires_at',
              'permit_owner_file','permit_owner_sha256','owner_session','coordination_root'}
    if set(value)!=expected or type(value['schema']) is not int or value['schema']!=1 or value['approved'] is not True or value['coordinator']!='/root':
        refuse()
    if value['purpose']!=purpose or value['lane']!='05' or type(value['cpu']) is not int or value['cpu']!=2:
        refuse()
    if value['source_packet_sha256']!=packet_sha or not re.fullmatch(r'[0-9a-f]{64}',packet_sha):
        refuse()
    if value['helper_sha256']!=helper_sha or value['build_receipt_sha256']!=build_sha:
        refuse()
    issued,expires=datetime.fromisoformat(value['issued_at']),datetime.fromisoformat(value['expires_at'])
    now=datetime.now(timezone.utc)
    if issued.tzinfo is None or expires.tzinfo is None or not issued<=now<expires or expires-issued>timedelta(minutes=45):
        refuse()
    coordination=Path(value['coordination_root'])
    if not coordination.is_absolute() or coordination.resolve(strict=True)!=coordination or not coordination.is_dir():
        refuse()
    owner_path=Path(value['permit_owner_file'])
    if owner_path not in [coordination/f'local-heavy-{number}.lock'/'owner.json' for number in [1,2,3]]:
        refuse()
    owner,owner_sha=public_json(owner_path)
    if owner_sha!=value['permit_owner_sha256'] or owner.get('lane')!='05' or type(owner.get('cpu')) is not int or owner.get('cpu')!=2 or owner.get('session')!=value['owner_session']:
        refuse()
    return {'admission_sha256':digest,'permit_owner_file':str(owner_path),'permit_owner_sha256':owner_sha,
            'owner_session':value['owner_session'],'purpose':purpose,'cpu':2}
