#!/usr/bin/env python3
"""Render reviewed builder resource sizes without changing the pinned install recipe."""
import argparse,hashlib,json,pathlib,re
p=argparse.ArgumentParser();p.add_argument('--source',required=True);p.add_argument('--source-sha256',required=True);p.add_argument('--output',required=True);p.add_argument('--cpus',type=int,required=True);p.add_argument('--memory-mib',type=int,required=True);p.add_argument('--disk-gib',type=int,required=True);a=p.parse_args()
raw=pathlib.Path(a.source).read_bytes();assert hashlib.sha256(raw).hexdigest()==a.source_sha256
assert 1<=a.cpus<=8 and 1024<=a.memory_mib<=16384 and 8<=a.disk_gib<=100
s=raw.decode()
for key,value in [('cpus',str(a.cpus)),('memory',(str(a.memory_mib//1024)+'GiB')if a.memory_mib%1024==0 else str(a.memory_mib)+'MiB'),('disk',str(a.disk_gib)+'GiB')]:
 s,n=re.subn('^'+key+':[^\n]*$',key+': '+value,s,count=1,flags=re.M);assert n==1
out=pathlib.Path(a.output);data=s.encode()
if out.exists():assert not out.is_symlink()and out.read_bytes()==data,'Existing reviewed recipe differs'
else:
 with out.open('xb')as f:f.write(data)
 out.chmod(0o600)
print(json.dumps({'source_sha256':a.source_sha256,'rendered_sha256':hashlib.sha256(data).hexdigest(),'cpus':a.cpus,'memory_mib':a.memory_mib,'disk_gib':a.disk_gib}))
