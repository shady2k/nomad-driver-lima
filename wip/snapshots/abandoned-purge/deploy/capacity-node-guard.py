"""Existing protected operator CLI; exact-node maintenance, no credentials export."""
import json,subprocess,sys
cli,node,mode=sys.argv[1:]
def api(path):return json.loads(subprocess.check_output([cli,'operator','api','-X','GET',path],stderr=subprocess.DEVNULL,timeout=20))
def state():
 n=api('/v1/node/'+node);rows=api('/v1/node/'+node+'/allocations?namespace=*')
 return {'node_id':n['ID'],'status':n['Status'],'eligibility':n['SchedulingEligibility'],'active_allocations':[a['ID'] for a in rows if a['ClientStatus'] not in ('complete','failed','lost')]}
s=state();assert s['node_id']==node
if mode=='disable':
 assert s['status']=='ready' and s['eligibility']=='eligible' and not s['active_allocations'],'Node not empty/ready/eligible'
 subprocess.run([cli,'node','eligibility','-disable',node],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,check=True,timeout=20)
 s=state();assert s['eligibility']=='ineligible' and not s['active_allocations'],'New allocation; do not restart'
elif mode=='enable':
 if s['eligibility']=='ineligible':subprocess.run([cli,'node','eligibility','-enable',node],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,check=True,timeout=20)
 s=state();assert s['eligibility']=='eligible'
elif mode!='read':raise ValueError('Unsupported guard action')
print(json.dumps(s))
