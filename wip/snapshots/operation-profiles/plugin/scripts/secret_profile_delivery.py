"""Driver-only one-shot delivery into existing task token tmpfs. stdin, no secrets."""
import json,os,pathlib,stat,sys,hashlib

def secret_kernel_identity():
 return {'machine_id':pathlib.Path('/etc/machine-id').read_text().strip(),'boot_id':pathlib.Path('/proc/sys/kernel/random/boot_id').read_text().strip()}

def deliver_secret_profiles(p):
 validate(p['catalogue'])
 if p['machine_identity']!=secret_kernel_identity():raise RuntimeError('SECRET_DELIVERY_MACHINE_CHANGED')
 root=secret_root(p['binding']['session_id'])
 filtered={'schema_version':1,'profiles':[v for v in p['catalogue']['profiles']if p['namespace']in v['scope']['namespaces']and p['task']in v['scope']['tasks']]}
 if not filtered['profiles']:return {'enabled':False}
 d={'catalogue':filtered,'catalogue_sha256':digest(filtered),'binding':p['binding'],'machine_identity':p['machine_identity'],'namespace':p['namespace'],'task':p['task'],'address':p['address'],'bao_bin':absolute(p['bao_bin'])}
 # Validate endpoint before storing. No token values are read or copied here.
 compile_supervisor(filtered['profiles'][0],p['address'],str(root/'vault_token'),[next(iter(filtered['profiles'][0]['clients'].values()))['executable']])
 with secret_lock(root):
  if any((root/x).exists()for x in ('sealed','secret-process.sealed')):raise RuntimeError('SECRET_RUNTIME_FINALIZED')
  path=root/'secret-profiles.json'
  if path.exists():
   if secret_read(path)!=d:raise RuntimeError('SECRET_DELIVERY_CHANGED_NO_REBIND')
  else:secret_atomic(path,d)
 return {'enabled':True,'catalogue_sha256':d['catalogue_sha256'],'profiles':len(filtered['profiles'])}

if __name__=='__main__':
 try:print(json.dumps({'ok':True,'result':deliver_secret_profiles(json.load(sys.stdin))}))
 except Exception:print('{"ok":false,"code":"SECRET_PROFILE_DELIVERY_REJECTED"}');sys.exit(1)
