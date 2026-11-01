"""Bounded nonsecret local guard; no environments/credentials or disk mutations."""
import hashlib,json,pathlib,subprocess,sys
c=json.loads(sys.argv[1]);p=pathlib.Path(c['binary']);assert not p.is_symlink() and p.is_file()
assert hashlib.sha256(p.read_bytes()).hexdigest() in (c['old_sha256'],c['new_sha256'])
assert hashlib.sha256(pathlib.Path(c['config']).read_bytes()).hexdigest()==c['config_sha256']
r=subprocess.run(['sudo','-u',c['runtime_user'],'env','LIMA_HOME='+c['lima_home'],c['limactl'],'list','--json'],capture_output=True,text=True,timeout=20);assert r.returncode==0
rows=[json.loads(x) for x in r.stdout.splitlines() if x.strip()];assert not any(v.get('status') not in ('Stopped',) for v in rows),'Non-stopped VM: no restart'
assert not any('qemu-system' in x for x in subprocess.check_output(['ps','-eo','comm='],text=True).splitlines()),'QEMU process exists'
print(json.dumps({'empty':True,'config_unchanged':True,'retained_instances':len(rows)}))
