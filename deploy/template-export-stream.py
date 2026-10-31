#!/usr/bin/env python3
"""Read-only exact accepted-artifact stream over the existing operator SSH.
No tar, arbitrary paths, credentials, directory listing or new endpoint.
"""
import hashlib,json,os,pathlib,re,stat,sys
sys.path.insert(0,str(pathlib.Path(__file__).parents[1]/'plugin/scripts'))
import exchange,template_save as save
FILES=('catalog.json','acceptance.json','export.json','metadata.json','disk.qcow2','disk.raw','runtime.yaml','model-catalog-proof.json')
def stream(payload,context,out):
 packet=payload['packet'];p=save.validate_operation(packet)
 if save.sha(p)!=packet['plan_sha256']:raise RuntimeError('TRANSFER_PLAN_CHANGED')
 root=pathlib.Path(context['build_root'])/p['operation_id']
 if root.is_symlink() or not root.is_dir():raise RuntimeError('STAGED_TRANSFER_ROOT_REQUIRED')
 if payload['action']=='probe':
  out.write((json.dumps({'operation_id':p['operation_id'],'plan_sha256':packet['plan_sha256'],'artifact_transport_binding_ready':True})+'\n').encode());return
 for stage in ('export','check','catalog'):save.verify_stage(stage,payload['receipts'][stage],packet)
 name=payload.get('file')
 if name not in FILES:raise RuntimeError('EXACT_ACCEPTED_ARTIFACT_FILE_REQUIRED')
 path=root/'artifact'/name;fd=os.open(path,os.O_RDONLY|os.O_NOFOLLOW)
 with os.fdopen(fd,'rb')as f:
  st=os.fstat(f.fileno())
  if not stat.S_ISREG(st.st_mode) or st.st_size>5*1024**3:raise RuntimeError('ARTIFACT_TRANSFER_BOUND')
  h=hashlib.sha256()
  for part in iter(lambda:f.read(1048576),b''):h.update(part)
  out.write((json.dumps({'operation_id':p['operation_id'],'plan_sha256':packet['plan_sha256'],'file':name,'size_bytes':st.st_size,'sha256':h.hexdigest()})+'\n').encode());f.seek(0)
  for part in iter(lambda:f.read(1048576),b''):out.write(part)
if __name__=='__main__':
 try:stream(json.load(sys.stdin),exchange.protected(sys.argv[1]),sys.stdout.buffer)
 except Exception as e:
  print(json.dumps({'error':str(e) if re.fullmatch('[A-Z_]{1,100}',str(e)) else type(e).__name__}),file=sys.stderr);sys.exit(1)
