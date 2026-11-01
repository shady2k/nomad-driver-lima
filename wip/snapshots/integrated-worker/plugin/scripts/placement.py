"""Pinned auto-platform planning. Capacity snapshot is not a reservation."""
import json,re

def variants(entries,template,version,arch,disk):
 out=[];seen=set()
 for e in entries:
  if (e.get('template_id'),e.get('version'))!=(template,version):continue
  if arch and e.get('architecture')!=arch:continue
  acc=e.get('acceptance',{})
  if not acc.get('sealed') or acc.get('independent_boots',0)<2 or not acc.get('unique_identities'):continue
  if e.get('guest_os')!='linux' or e.get('architecture') not in ('amd64','arm64') or e.get('vm_type') not in ('qemu','vz'):continue
  key=(e['architecture'],e['vm_type'])
  if key in seen:raise ValueError('DUPLICATE_PLATFORM_VARIANT')
  seen.add(key);d=e.get('disk',{});y=e.get('runtime_yaml',{});minimum=d.get('minimum_disk_gib')
  if type(minimum)!=int or minimum<1:raise ValueError('INVALID_IMAGE_MINIMUM')
  if disk<minimum:continue
  if any(not isinstance(v,str) or not re.fullmatch('[a-f0-9]{64}',v) for v in (d.get('sha256'),y.get('sha256'))):raise ValueError('PINNED_VARIANT_HASH_REQUIRED')
  out.append({'architecture':e['architecture'],'guest_os':'linux','vm_type':e['vm_type'],'disk_sha256':d['sha256'],'yaml_sha256':y['sha256'],'minimum_disk_gib':minimum})
 if len(out)>32:raise ValueError('TOO_MANY_PLATFORM_VARIANTS')
 if not out:raise ValueError('NO_ACCEPTED_COMPATIBLE_VARIANT')
 return sorted(out,key=lambda e:(e['architecture'],e['vm_type']))

def envelope(pins,nodes,cpus,memory,disk,catalog_sha256):
 eligible=[]
 for n in nodes:
  vm='vz' if n.get('host_os')=='darwin' else 'qemu' if n.get('host_os')=='linux' else None
  if not n.get('placement_eligible',False) or not n.get('late_platform_selection') or n.get('catalog_sha256')!=catalog_sha256 or not any((p['architecture'],p['vm_type'])==(n.get('architecture'),vm) for p in pins):continue
  per=n.get('cpu_mhz_per_vcpu');over=n.get('memory_overhead_mib_per_vm');a=n.get('available_for_one_new_vm') or {}
  if not isinstance(n.get('node_id'),str) or not re.fullmatch('[-A-Za-z0-9]{1,64}',n['node_id']) or type(per)!=int or per<1 or type(over)!=int or over<1:continue
  if any(type(a.get(k))!=int for k in ('vcpus','guest_memory_mib','disk_gib')):continue
  eligible.append(n)
 if not eligible:raise RuntimeError('ELIGIBLE_PLATFORM_TELEMETRY_UNAVAILABLE')
 per=max(n['cpu_mhz_per_vcpu'] for n in eligible);over=max(n['memory_overhead_mib_per_vm'] for n in eligible);cpu=cpus*per;mem=memory+over;fits=[]
 for n in eligible:
  a=n['available_for_one_new_vm'];fit=(a['vcpus']>=cpus and a['guest_memory_mib']>=memory and a['disk_gib']>=disk and a.get('additional_vm_slots')!=0 and n.get('free_cpu_mhz',-1)>=cpu and n.get('free_reservation_mib',-1)>=mem)
  fits.append({'node_id':n['node_id'],'architecture':n['architecture'],'fits_snapshot_envelope':fit})
 if not any(n['fits_snapshot_envelope'] for n in fits):raise RuntimeError('CONSERVATIVE_RESERVATION_ENVELOPE_NO_CURRENT_FIT')
 return {'cpu_mhz':cpu,'reservation_mib':mem,'compute_per_core':per,'overhead_mib':over,'candidate_node_ids':sorted(n['node_id'] for n in eligible),'per_node_fit':fits,'overreservation_possible':True,'advisory':True}

def selected(expected,allocation,task):
 rows=[]
 for event in allocation.get('TaskStates',{}).get(task,{}).get('Events',[]):
  d=event.get('Details',{})
  if 'template_catalog_sha256' not in d:continue
  if d['template_catalog_sha256']!=expected['catalog_sha256']:raise RuntimeError('PLACED_CATALOG_CHANGED')
  v={'architecture':d.get('resolved_architecture'),'guest_os':d.get('resolved_guest_os'),'vm_type':d.get('resolved_vm_type'),'disk_sha256':d.get('template_disk_sha256'),'yaml_sha256':d.get('template_yaml_sha256')}
  matches=[p for p in expected['variants'] if all(p[k]==x for k,x in v.items())]
  if len(matches)!=1:raise RuntimeError('PLACED_VARIANT_NOT_PINNED')
  rows.append({'catalog_sha256':expected['catalog_sha256'],**v})
 if not rows:return None
 if any(row!=rows[0] for row in rows):raise RuntimeError('PLACED_VARIANT_CHANGED_NO_RESELECT')
 return rows[0]

def allocation_attempt(a,task):
 t=a.get('TaskStates',{}).get(task,{})
 return {'allocation':a['ID'],'node_id':a.get('NodeID'),'job_version':a.get('Job',{}).get('Version'),'restarts':t.get('Restarts',0),'started_at':t.get('StartedAt')}
