"""Bounded deployment archival operation; no service, disk purge or credential read."""
import json,os,pathlib,re,stat,sys
items=json.loads(sys.argv[1]);archive=pathlib.Path(sys.argv[2]);check=sys.argv[3]=='check'
assert archive.is_absolute() and (check or archive.is_dir())
work=[]
for item in items:
    src=pathlib.Path(item['src']);name=item['name'];dst=archive/name
    assert re.fullmatch(r'[A-Za-z0-9_.-]+',name)
    assert src.is_absolute() and '..' not in src.parts
    assert not set(src.parts).intersection({'runtime','boxes','images','credentials','sessions','driver-state','nomad'})
    assert src.name in {'boxlite','boxlite-stdio','nomad-driver-boxlite','serve.py','cleanup-stopped.py','read-rest-key.py','custom','boxlite-0.10.8'} or src.name.startswith('boxlite-rest.service')
    assert archive.resolve()!=src.resolve() and src.resolve() not in archive.resolve().parents
    exists=os.path.lexists(src);archived=os.path.lexists(dst)
    assert not (exists and archived),'Source and archive both exist; refuse overwrite'
    if exists:
        s=src.lstat();work.append({'src':str(src),'destination':str(dst),'uid':s.st_uid,'gid':s.st_gid,'mode':oct(stat.S_IMODE(s.st_mode)),'logical_bytes':s.st_size})
if not check:
    for item in work:os.rename(item['src'],item['destination'])
    if work:
        p=archive/'component-moves.json';prior=json.loads(p.read_text()) if p.exists() else []
        p.write_text(json.dumps(prior+work,indent=2)+'\n');p.chmod(0o600)
print(json.dumps({'changed':bool(work),'check_mode':check,'component_count':len(work),'archive':str(archive),'data_deleted':False}))
