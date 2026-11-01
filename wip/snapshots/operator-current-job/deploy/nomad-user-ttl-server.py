"""Exact approved server ACL TTL bound, selected safe diagnostics only."""
import json,sys,re
def main():
 """Exact approved server ACL TTL cap; no client/token/provider mutation."""
 import json,hashlib,pathlib,re,os,tempfile,subprocess
 c=TTL_SERVER_CONFIG
 assert set(c)=={'namespace','configmap','statefulset','pod','config_key','source_manifest'}
 def run(args,data=None):
  p=subprocess.run(['/opt/homebrew/bin/kubectl',*args],input=data,capture_output=True,text=True,timeout=40)
  if p.returncode:raise RuntimeError('CANDIDATE_VALIDATION_FAILED' if 'exec' in args else ('CONFIGMAP_PATCH_FAILED' if 'configmap' in args and 'patch' in args else 'NOMAD_TTL_KUBERNETES_OPERATION_FAILED'))
  return p.stdout
 def get(kind,name):return json.loads(run(['-n',c['namespace'],'get',kind,name,'-o','json']))
 def checksum(x):return hashlib.sha256(x.encode()).hexdigest()
 def source_config():
  p=pathlib.Path(c['source_manifest']);lines=p.read_text().splitlines(keepends=True)
  start=next(i for i,x in enumerate(lines) if re.match(r'\s*'+re.escape(c['config_key'])+r':\s*\|',x))
  index=start+1;raw=lines[index].strip();return p,lines,index,json.loads(raw)
 cm=get('configmap',c['configmap']);sts=get('statefulset',c['statefulset']);server=json.loads(cm['data'][c['config_key']])
 assert server.get('acl',{}).get('token_max_expiration_ttl') in (None,'720h')
 p,lines,index,source=source_config();assert source.get('acl',{}).get('token_max_expiration_ttl') in (None,'720h')
 before={'config_resource_version':cm['metadata']['resourceVersion'],'server_json':server,'source_server_json':source,'template_annotations':sts['spec']['template']['metadata'].get('annotations',{}),'pod_uid':get('pod',c['pod'])['metadata']['uid']}
 if TTL_SERVER_MODE=='inspect':print('TTL_SERVER_BEFORE:'+json.dumps({'guard_sha256':checksum(json.dumps(before,sort_keys=True)),'snapshot':before}))
 elif TTL_SERVER_MODE=='apply':
  assert checksum(json.dumps(before,sort_keys=True))==EXPECTED_SERVER_GUARD,'Concurrent server/source change; inspect before retry'
  desired=json.loads(json.dumps(server));desired.setdefault('acl',{})['token_max_expiration_ttl']='720h'
  comparison=json.loads(json.dumps(desired));comparison['acl'].pop('token_max_expiration_ttl')
  expected=json.loads(json.dumps(server));expected['acl'].pop('token_max_expiration_ttl',None)
  assert comparison==expected
  encoded=json.dumps(desired,separators=(',',':'));digest=checksum(encoded)
  changed_config=server!=desired
  # Validate exact candidate in existing server container without activation.
  validate='candidate="$(mktemp /tmp/nomad-user-ttl.XXXXXX)"\ntrap \'rm -f "$candidate"\' EXIT\ncat > "$candidate"\nnomad config validate "$candidate"'
  run(['-n',c['namespace'],'exec','-i',c['pod'],'--','sh','-ec',validate],encoded)
  if changed_config:
   patch=[{'op':'test','path':'/metadata/resourceVersion','value':cm['metadata']['resourceVersion']},{'op':'test','path':'/data/'+c['config_key'],'value':cm['data'][c['config_key']]},{'op':'replace','path':'/data/'+c['config_key'],'value':encoded}]
   run(['-n',c['namespace'],'patch','configmap',c['configmap'],'--type=json','--patch-file=/dev/stdin'],json.dumps(patch))
  # Reconcile exactly the same field in the existing private source document.
  source_desired=json.loads(json.dumps(source));source_desired.setdefault('acl',{})['token_max_expiration_ttl']='720h'
  changed_source=source!=source_desired
  if changed_source:
   lines[index]=re.match(r'^\s*',lines[index])[0]+json.dumps(source_desired,separators=(',',':'))+'\n'
   st=p.stat();fd,tmp=tempfile.mkstemp(prefix='.nomad-user-ttl-',dir=p.parent)
   try:
    with os.fdopen(fd,'w') as f:os.fchmod(f.fileno(),st.st_mode&0o777);f.writelines(lines);f.flush();os.fsync(f.fileno())
    os.replace(tmp,p)
   finally:
    if os.path.exists(tmp):os.unlink(tmp)
  annotation='operator.nomad/user-acl-ttl-config-sha256';restart=before['template_annotations'].get(annotation)!=digest
  if restart:
   latest=get('statefulset',c['statefulset'])
   patch=[{'op':'test','path':'/metadata/resourceVersion','value':latest['metadata']['resourceVersion']}]
   if 'annotations' not in latest['spec']['template']['metadata']:patch.append({'op':'add','path':'/spec/template/metadata/annotations','value':{}})
   patch.append({'op':'add','path':'/spec/template/metadata/annotations/'+annotation.replace('/','~1'),'value':digest})
   run(['-n',c['namespace'],'patch','statefulset',c['statefulset'],'--type=json','--patch-file=/dev/stdin'],json.dumps(patch))
  print('TTL_SERVER_RESULT:'+json.dumps({'changed':changed_config or changed_source or restart,'config_changed':changed_config,'source_changed':changed_source,'server_rollout_requested':restart,'desired_cap':'720h','config_sha256':digest,'clients_modified':False,'other_server_settings_unchanged':True}))
 else:raise RuntimeError('Unsupported mode')

try:main()
except Exception as e:
 reason=str(e) if re.fullmatch('[A-Z_]{1,100}',str(e)) else type(e).__name__
 print('TTL_SERVER_ERROR:'+json.dumps({'code':reason}))
 raise SystemExit(1)
