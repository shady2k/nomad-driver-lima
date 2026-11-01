"""Operator-side namespace desired state. Existing token stays on operator host."""
import copy,datetime,json,pathlib,sys,urllib.error,urllib.request
base,token_file,source,target,backup,mode=sys.argv[1:]
assert source!=target and target=='agent-workers'
token=json.loads(pathlib.Path(token_file).read_text())['SecretID']
def call(path,body=None):
    req=urllib.request.Request(base+path,headers={'X-Nomad-Token':token,'Content-Type':'application/json'},data=None if body is None else json.dumps(body).encode(),method='GET' if body is None else 'POST')
    with urllib.request.urlopen(req,timeout=20) as r:
        raw=r.read();return json.loads(raw) if raw else None
def spec(value):return {k:v for k,v in value.items() if k not in ['Hash','CreateIndex','ModifyIndex']}
original=call('/v1/namespace/'+source);desired=spec(copy.deepcopy(original));desired['Name']=target;desired['Description']='Agent workers using the Lima VM backend';desired.setdefault('Capabilities',{})['EnabledTaskDrivers']=['lima']
try:current=call('/v1/namespace/'+target)
except urllib.error.HTTPError as e:
    if e.code!=404:raise
    current=None
changed=current is None or spec(current)!=desired
if changed and mode!='check':
    root=pathlib.Path(backup);root.mkdir(mode=0o700,parents=True,exist_ok=True)
    p=root/'namespace.before.json'
    if not p.exists():p.write_text(json.dumps({'source':original,'target':current},indent=2));p.chmod(0o600)
    call('/v1/namespace/'+target,desired)
    after=call('/v1/namespace/'+target);assert spec(after)==desired,'Namespace readback mismatch'
    p=root/'namespace.after.json';p.write_text(json.dumps(after,indent=2));p.chmod(0o600)
else:after=current
print(json.dumps({'changed':changed,'namespace':target,'check_mode':mode=='check','enabled_drivers':desired['Capabilities']['EnabledTaskDrivers'],'disabled_drivers':desired['Capabilities'].get('DisabledTaskDrivers'),'acl_changed':False,'old_namespace_changed':False,'readback_verified':mode!='check'}))
