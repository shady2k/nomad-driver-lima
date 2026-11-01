#!/usr/bin/env python3
"""Convert an approved prepared-copy observation to private pinned inputs. Never deploy."""
import argparse,hashlib,json,os,pathlib,re,uuid

def descriptor(report,node,namespace,cap):
 if str(uuid.UUID(node))!=node or not re.fullmatch('[a-z0-9][a-z0-9-]{0,63}',namespace)or type(cap)!=int or cap<1:raise ValueError('explicit node/namespace/positive retained disk cap required')
 if report.get('status')!='prepared_only_no_restore':raise ValueError('prepared-only observation required')
 inputs={}
 for s in report['sources']:
  o=s['owner_evidence'];name='recover-'+s['label']
  if not re.fullmatch('[a-z0-9][a-z0-9-]{0,31}',name)or name in inputs:raise ValueError('unique recovery label required')
  if any(s.get(k)is not True for k in ('source_unchanged','source_stopped','standalone','qemu_check_passed'))or s.get('boot_executed')is not False or s.get('runtime_cleanup_executed')is not False:raise ValueError('unchanged stopped standalone prepared copy required')
  if o.get('native_record_matches')is not True or o.get('native_record_ended')is not False or (o['creator_agent_id'],o['creator_session_id'],o['session'],o['allocation'])!=(s['source_creator_agent'],s['source_creator_session'],s['source_session'],s['allocation']):raise ValueError('exact current native owner evidence required')
  for k in ('creator_agent_id','creator_generation','allocation','session'):
   if str(uuid.UUID(o[k]))!=o[k]:raise ValueError('canonical identity required')
  if not re.fullmatch('[a-f0-9]{64}',s['copy_sha256'])or not re.fullmatch('[a-f0-9]{64}',s['source_key'])or s['copy_size_bytes']!=s['source_stat']['size_bytes']:raise ValueError('copy/source binding required')
  if not pathlib.PurePosixPath(s['copy_path']).is_absolute()or '..'in pathlib.PurePosixPath(s['copy_path']).parts:raise ValueError('absolute copy path required')
  inputs[name]={'id':name,'node_id':node,'namespace':namespace,'creator_agent_id':o['creator_agent_id'],'creator_session_id':o['creator_session_id'],'creator_generation':o['creator_generation'],'source_key':s['source_key'],'source_allocation':s['allocation'],'source_session':s['source_session'],'source_instance':s['source_instance'],'copy_path':s['copy_path'],'copy_sha256':s['copy_sha256'],'copy_size_bytes':s['copy_size_bytes'],'virtual_size_bytes':s['virtual_size_bytes'],'architecture':'amd64','workspace':o['guest_workspace'],'guest_state_dir':o['guest_state_dir'],'operator_approved':True}
 if not 1<=len(inputs)<=8:raise ValueError('bounded input set required')
 return {'schema_version':1,'max_reserved_disk_gib':cap,'inputs':inputs}

def main():
 p=argparse.ArgumentParser(description=__doc__);p.add_argument('--prepared-observation',required=True,type=pathlib.Path);p.add_argument('--node-id',required=True);p.add_argument('--namespace',required=True);p.add_argument('--max-reserved-disk-gib',required=True,type=int);p.add_argument('--output',required=True,type=pathlib.Path);a=p.parse_args()
 raw=json.dumps(descriptor(json.loads(a.prepared_observation.read_text()),a.node_id,a.namespace,a.max_reserved_disk_gib),sort_keys=True,separators=(',',':')).encode()
 fd=os.open(a.output,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
 with os.fdopen(fd,'wb')as f:f.write(raw);f.flush();os.fsync(f.fileno())
 print(json.dumps({'path':str(a.output),'sha256':hashlib.sha256(raw).hexdigest(),'status':'prepared_not_installed'}))
if __name__=='__main__':main()
