"""Agent-facing catalog/admission client. Nomad/driver remain authoritative."""
import hashlib,json,os,pathlib,re,subprocess,tempfile,time,uuid

def canonical(v):return json.dumps(v,sort_keys=True,separators=(',',':')).encode()
def atomic(p,v):
 p=pathlib.Path(p);p.parent.mkdir(parents=True,exist_ok=True,mode=0o700)
 fd,tmp=tempfile.mkstemp(prefix='.intent-',dir=p.parent)
 try:
  with os.fdopen(fd,'wb') as f:os.fchmod(f.fileno(),0o600);f.write(canonical(v));f.flush();os.fsync(f.fileno())
  os.replace(tmp,p)
 finally:
  if os.path.exists(tmp):os.unlink(tmp)
def uid(s):
 if str(uuid.UUID(s))!=s:raise ValueError('canonical session UUID required')
 return s
class CatalogClient:
 def __init__(self,config,read_only=False):
  self.config=pathlib.Path(config);self.c=json.loads(self.config.read_text());self.root=pathlib.Path(self.c['receipt_root']);
  if read_only:
   if not self.root.is_dir():raise ValueError('EXISTING_RECEIPT_ROOT_REQUIRED')
  else:self.root.mkdir(parents=True,exist_ok=True,mode=0o700)
  if self.root.is_symlink() or self.root.stat().st_mode&0o077:raise ValueError('private receipt_root required')
  self.bin=self.c.get('nomad_bin','nomad')
 def run(self,args,input=None,timeout=None):return subprocess.run([self.bin,*args],input=input,capture_output=True,**({'timeout':timeout}if timeout is not None else{}))
 def api(self,path):
  r=self.run(['operator','api','-X','GET',path])
  if r.returncode:raise RuntimeError('NOMAD_READ_UNAVAILABLE_OR_ACL_DENIED')
  try:return json.loads(r.stdout)
  except (ValueError,UnicodeDecodeError):raise RuntimeError('NOMAD_READ_INVALID_JSON') from None
 def templates(self):
  import catalog_routing
  if catalog_routing.enabled(self.c):return catalog_routing.metadata(self.c)
  raw=pathlib.Path(self.c['catalog_file']).read_bytes()
  if len(raw)>4194304 or hashlib.sha256(raw).hexdigest()!=self.c['catalog_sha256']:raise ValueError('CATALOG_CHECKSUM_MISMATCH')
  doc=json.loads(raw)
  if doc.get('schema_version')!=1:raise ValueError('CATALOG_VERSION')
  # Metadata only, never return artifact URLs (may contain a temporary signature).
  return [{k:v for k,v in e.items() if k in ('template_id','version','architecture','vm_type','guest_os','tools','acceptance')}|{'disk':{k:v for k,v in e['disk'].items() if k!='url'},'runtime_yaml':{k:v for k,v in e.get('runtime_yaml',{}).items() if k!='url'}} for e in doc['templates']]
 def capacity(self):
  out=[]
  for n in self.api('/v1/nodes?resources=true'):
   reasons=[]
   if n.get('Status')!='ready':reasons.append('node_not_ready')
   if n.get('SchedulingEligibility')!='eligible':reasons.append('node_not_eligible')
   try:d=self.api('/v1/node/'+n['ID'])
   except RuntimeError:
    out.append({'node_id':n['ID'],'name':n.get('Name'),'node_status':n.get('Status'),'scheduling_eligibility':n.get('SchedulingEligibility'),'placement_eligible':False,'placement_reasons':reasons+['node_detail_unavailable'],'available_for_one_new_vm':None,'advisory':True});continue
   drv=d.get('Drivers',{}).get('lima',{})
   if not drv.get('Detected'):reasons.append('lima_not_detected')
   elif not drv.get('Healthy'):reasons.append('lima_unhealthy')
   try:rows=self.api('/v1/node/'+n['ID']+'/allocations')
   except RuntimeError:
    out.append({'node_id':d['ID'],'name':d.get('Name'),'architecture':d.get('Attributes',{}).get('cpu.arch'),'host_os':d.get('Attributes',{}).get('kernel.name'),'node_status':n.get('Status'),'scheduling_eligibility':n.get('SchedulingEligibility'),'placement_eligible':False,'placement_reasons':reasons+['allocation_usage_unavailable'],'available_for_one_new_vm':None,'advisory':True});continue
   used_cpu=used_mem=0
   for a in rows:
    if a.get('ClientStatus') in ('complete','failed','lost'):continue
    tasks=a.get('AllocatedResources',{}).get('Tasks',{})
    if not tasks:
     full=self.api('/v1/allocation/'+a['ID']);tasks=full.get('AllocatedResources',{}).get('Tasks',{})
    for t in tasks.values():used_cpu+=t.get('Cpu',{}).get('CpuShares',0);used_mem+=t.get('Memory',{}).get('MemoryMB',0)
   attrs=d.get('Attributes',{});da=drv.get('Attributes',{});nr=d.get('NodeResources',{});reserved=d.get('ReservedResources',{})
   cpu=nr.get('Cpu',{}).get('CpuShares',0)-reserved.get('Cpu',{}).get('CpuShares',0);mem=nr.get('Memory',{}).get('MemoryMB',0)-reserved.get('Memory',{}).get('MemoryMB',0)
   def value(k):
    v=da.get(k,attrs.get(k));v=v.get('IntVal',v.get('StringVal',v.get('BoolVal'))) if isinstance(v,dict) else v
    return int(v) if isinstance(v,str) and re.fullmatch(r'-?[0-9]+',v) else v
   per=value('driver.lima.cpu.nomad_compute_per_core')
   try:per=int(per)
   except (ValueError,TypeError):per=0
   from capacity_view import available
   limits={k:value('driver.lima.operator_limits.'+k) for k in ('max_vms','max_total_vcpus','max_total_memory_mib','max_total_disk_gib','max_guest_vcpus','max_guest_memory_mib','max_guest_disk_gib','memory_overhead_mib')}
   telemetry={k:value('driver.lima.capacity.'+k) for k in ('known','observed_unix','used_vms','used_vcpus','used_reservation_mib','used_disk_gib','physical_available_mib','disk_floor_mib')}
   # Nomad serializes boolean attributes separately from integer/string values.
   known=da.get('driver.lima.capacity.known',attrs.get('driver.lima.capacity.known'))
   telemetry['known']=known.get('BoolVal') if isinstance(known,dict) else known in (True,'true')
   overhead=limits.get('memory_overhead_mib')
   effective=available(max(0,cpu-used_cpu),max(0,mem-used_mem),per,overhead if type(overhead)==int else self.c.get('memory_overhead_mib',0),limits,telemetry)
   if per<=0:reasons.append('lima_cpu_conversion_unknown')
   if reasons:effective={'vcpus':None,'guest_memory_mib':None,'disk_gib':None,'additional_vm_slots':None,'telemetry':'ineligible_or_unknown','advisory':True}
   out.append({'node_status':n.get('Status'),'scheduling_eligibility':n.get('SchedulingEligibility'),'repository_profiles':{k.removeprefix('driver.lima.repositories.profile.'):value(k)for k in set(attrs)|set(da)if k.startswith('driver.lima.repositories.profile.')},'repository_profile_default':value('driver.lima.repositories.default'),'driver':{'name':'lima','detected':bool(drv.get('Detected')),'healthy':bool(drv.get('Healthy'))},'placement_eligible':not reasons,'placement_reasons':reasons,'late_platform_selection':value('driver.lima.catalog.late_platform_selection') in (True,'true'),'catalog_sha256':value('driver.lima.catalog.sha256'),'available_for_one_new_vm':effective,'occupied':{'cpu_mhz':used_cpu,'reservation_mib':used_mem,'driver':telemetry},'node_id':d['ID'],'name':d.get('Name'),'architecture':attrs.get('cpu.arch'),'host_os':attrs.get('kernel.name'),'cpu_mhz_per_vcpu':per,'allocatable_cpu_mhz':max(0,cpu),'used_cpu_mhz':used_cpu,'free_cpu_mhz':max(0,cpu-used_cpu),'free_guest_vcpus_by_cpu':max(0,cpu-used_cpu)//per if per>0 and not reasons else None,'memory_overhead_mib_per_vm':value('driver.lima.operator_limits.memory_overhead_mib'),'free_guest_memory_mib_for_one_vm':None if reasons else max(0,mem-used_mem-int(value('driver.lima.operator_limits.memory_overhead_mib') or self.c.get('memory_overhead_mib',0))),'allocatable_reservation_mib':max(0,mem),'used_reservation_mib':used_mem,'free_reservation_mib':max(0,mem-used_mem),'disk_free':'unknown_until_authoritative_driver_admission','node_operator_limits':{k:value('driver.lima.operator_limits.'+k) for k in ('max_vms','max_total_vcpus','max_total_memory_mib','max_total_disk_gib','max_guest_vcpus','max_guest_memory_mib','max_guest_disk_gib','memory_overhead_mib')},'advisory':True})
  from capacity_view import describe,summarize
  result={'observed_unix':int(__import__('time').time()),'snapshot_not_reservation':True,'units':{'cpu':'whole_vcpus','memory':'MiB_guest_plus_explicit_overhead','disk':'GiB_admission_quota'},'nodes':out,'resource_scope':'per_node_not_interchangeable_or_summed','operator_limits':self.c.get('request_limits',{}),'quota_authority':'Nomad reservations and configured driver node budgets; displayed free capacity is advisory','auto_close_enabled':False}
  import catalog_routing
  if catalog_routing.enabled(self.c):result=catalog_routing.capacity(result,self.templates())
  describe(result['nodes'])
  return summarize(result)
 def request_templates(self):
  entries=self.templates()
  if self.c.get('model_catalog_file'):
   import model_catalog
   model_catalog.load(self.c)
   entries=[e for e in entries if e.get('tools',{}).get('prime_model_catalog')=='1']
   if not entries:raise ValueError('MODEL_CATALOG_IMAGE_REQUIRED')
  return entries
 def capacity_request(self,template,version,arch,cpus,memory,disk,repository_profile=None,repository_profile_sha256=None):
  import placement
  if any(type(v)!=int or v<1 for v in (cpus,memory,disk)):raise ValueError('POSITIVE_REQUEST_RESOURCES_REQUIRED')
  arch=None if arch in (None,'auto') else arch
  if arch not in (None,'amd64','arm64'):raise ValueError('UNSUPPORTED_REQUEST_ARCHITECTURE')
  result=self.capacity()
  import catalog_routing
  if catalog_routing.enabled(self.c):
   route=catalog_routing.plan(self.request_templates(),self.repository_nodes(result['nodes'],repository_profile,repository_profile_sha256),template,version,arch,cpus,memory,disk)
   result['auto_placement_request_fit']=dict(route['reservation_envelope'],catalog_source=route['catalog_source'],catalog_sha256=route['catalog_sha256'],selection_policy=route['selection_policy'],catalog_routes=route['catalog_routes'])
   return result
  pins=placement.variants(self.request_templates(),template,version,arch,disk)
  result['auto_placement_request_fit']=placement.envelope(pins,self.repository_nodes(result['nodes'],repository_profile,repository_profile_sha256),cpus,memory,disk,self.c['catalog_sha256'])
  return result
 def repository_nodes(self,nodes,profile,sha=None):
  if profile is None:
   if sha is not None:raise ValueError('REPOSITORY_PROFILE_ID_REQUIRED')
   return nodes
  if not isinstance(profile,str)or not re.fullmatch('[a-z0-9][a-z0-9-]{0,63}',profile):raise ValueError('REPOSITORY_PROFILE_ID_REQUIRED')
  if sha is not None and (not isinstance(sha,str)or not re.fullmatch('[a-f0-9]{64}',sha)):raise ValueError('REPOSITORY_PROFILE_DIGEST_REQUIRED')
  rows=[n for n in nodes if re.fullmatch('[a-f0-9]{64}',n.get('repository_profiles',{}).get(profile,''))and (sha is None or n['repository_profiles'][profile]==sha)]
  if not rows:raise RuntimeError('REPOSITORY_PROFILE_NODE_UNAVAILABLE')
  return rows
 def request(self,session,template,version,arch,cpus,memory,disk,purpose,project='',link='',creator_record=None,model=None,task_title=None,source_agent_name=None,source_agent_type=None,repository_profile=None,repository_profile_sha256=None):
  import fcntl
  root=self.root/uid(session);root.mkdir(exist_ok=True,mode=0o700)
  with open(root/'request.lock','a') as lock:
   os.chmod(lock.name,0o600);fcntl.flock(lock,fcntl.LOCK_EX)
   return self._request_locked(session,template,version,arch,cpus,memory,disk,purpose,project,link,creator_record,model,task_title,source_agent_name,source_agent_type,repository_profile,repository_profile_sha256)
 def _request_locked(self,session,template,version,arch,cpus,memory,disk,purpose,project='',link='',creator_record=None,model=None,task_title=None,source_agent_name=None,source_agent_type=None,repository_profile=None,repository_profile_sha256=None):
  uid(session);uid(self.c['owner_id']);uid(self.c['agent_instance_id'])
  creator_session=self.c.get('creator_session_id',session);agent=self.c['agent_instance_id'];generation=None;native={}
  if creator_record:
   import exchange
   native=exchange.protected(creator_record)
   if native.get('ended') or native.get('evidence')!='native_hook_observation_not_authenticated_liveness':raise ValueError('INITIATOR_RECORD_NOT_ACTIVE')
   uid(native['agent_instance_id']);uid(native['generation']);creator_session=native['native_session_id'];agent=native['agent_instance_id'];generation=native['generation']
  if not re.fullmatch('[A-Za-z0-9][A-Za-z0-9._:-]{0,255}',creator_session):raise ValueError('CREATOR_SESSION_ID_INVALID')
  if not re.fullmatch('[a-z0-9][a-z0-9-]{0,31}',purpose) or (project and not re.fullmatch('[a-z0-9][a-z0-9-]{0,31}',project)):raise ValueError('PROJECT_PURPOSE_SLUG_REQUIRED: use 1-32 lowercase letters, digits or hyphens, starting with a letter or digit; no spaces')
  if len(link)>1024 or any(c in link for c in '\r\n\x00'):raise ValueError('invalid task link')
  if any(type(x)!=int or x<1 for x in (cpus,memory,disk)):raise ValueError('positive integer resources required')
  limits=self.c.get('request_limits',{})
  for k,n in [('vcpus',cpus),('memory_mib',memory),('disk_gib',disk)]:
   if limits.get(k,0)>0 and n>limits[k]:raise ValueError('REQUEST_EXCEEDS_OPERATOR_POLICY')
  arch=None if arch in (None,'auto') else arch
  if arch not in (None,'amd64','arm64'):raise ValueError('UNSUPPORTED_REQUEST_ARCHITECTURE')
  root=self.root/session;p=root/'allocation-intent.json'
  request_input={'config':self.c,'session':session,'template':template,'version':version,'architecture':arch,'vcpus':cpus,'memory_mib':memory,'disk_gib':disk,'purpose':purpose,'project':project,'link':link,'creator_agent':agent,'creator_session':creator_session,'creator_generation':generation,'coordinator_model':model}
  if repository_profile is not None or repository_profile_sha256 is not None:request_input['repository_profile']={'id':repository_profile,'sha256':repository_profile_sha256}
  input_hash=hashlib.sha256(canonical(request_input)).hexdigest()
  if p.exists():
   previous=json.loads(p.read_text())
   if previous.get('descriptive_provenance') is not None:
    from provenance import descriptions
    described=descriptions(self.c,native,task_title,source_agent_name,source_agent_type,link,purpose)
    input_hash=hashlib.sha256(canonical({'legacy_input_hash':input_hash,'descriptive_provenance':described})).hexdigest()
   if 'input_hash' in previous:
    if previous['input_hash']!=input_hash:
     # Adding optional discovery sources must not replan an already persisted
     # legacy request. Every other byte of its original request must still match.
     legacy_config={k:v for k,v in self.c.items()if k!='catalog_sources'}
     legacy_hash=hashlib.sha256(canonical(dict(request_input,config=legacy_config))).hexdigest()
     if previous.get('descriptive_provenance') is not None:legacy_hash=hashlib.sha256(canonical({'legacy_input_hash':legacy_hash,'descriptive_provenance':described})).hexdigest()
     if 'catalog_sources'not in self.c or legacy_hash!=previous['input_hash']:raise ValueError('SESSION_REQUEST_CHANGED')
    return self.request_status(session)
  from provenance import descriptions
  described=descriptions(self.c,native,task_title,source_agent_name,source_agent_type,link,purpose)
  input_hash=hashlib.sha256(canonical({'legacy_input_hash':input_hash,'descriptive_provenance':described})).hexdigest()
  guest_config=None
  if self.c.get('model_catalog_file') or model is not None:
   import model_catalog
   guest_config=model_catalog.select_guest(self.c,model)
  approved=self.request_templates()
  placement_plan=None;pins=None;catalog_route=None;selected_catalog=self.c['catalog_sha256']
  import catalog_routing
  if catalog_routing.enabled(self.c):
   catalog_route=catalog_routing.plan(approved,self.repository_nodes(self.capacity()['nodes'],repository_profile,repository_profile_sha256),template,version,arch,cpus,memory,disk)
   pins=catalog_route['variants'];placement_plan=catalog_route['reservation_envelope'];selected_catalog=catalog_route['catalog_sha256']
   per=placement_plan['compute_per_core'];overhead=placement_plan['overhead_mib']
  elif arch is None:
   import placement
   pins=placement.variants(approved,template,version,None,disk)
   placement_plan=placement.envelope(pins,self.repository_nodes(self.capacity()['nodes'],repository_profile,repository_profile_sha256),cpus,memory,disk,self.c['catalog_sha256'])
   per=placement_plan['compute_per_core'];overhead=placement_plan['overhead_mib']
  else:
   entries=[e for e in approved if (e['template_id'],e['version'],e['architecture'])==(template,version,arch)]
   if not entries:raise ValueError('APPROVED_TEMPLATE_UNAVAILABLE')
   if len(entries)!=1:raise ValueError('EXPLICIT_PLATFORM_REQUIRED_FOR_AMBIGUOUS_TEMPLATE')
   entry=entries[0];acc=entry.get('acceptance',{})
   if not acc.get('sealed') or acc.get('independent_boots',0)<2 or not acc.get('unique_identities'):raise ValueError('TEMPLATE_ACCEPTANCE_PENDING')
   if disk<entry['disk']['minimum_disk_gib']:raise ValueError('DISK_BELOW_IMAGE_MINIMUM')
   rows=[n for n in self.repository_nodes(self.capacity()['nodes'],repository_profile,repository_profile_sha256) if n.get('placement_eligible',True) and n.get('architecture')==arch and n.get('host_os')==('darwin' if entry['vm_type']=='vz' else 'linux') and n.get('cpu_mhz_per_vcpu',0)>0]
   if not rows:raise RuntimeError('ELIGIBLE_NODE_INVENTORY_UNAVAILABLE')
   per=max(n['cpu_mhz_per_vcpu'] for n in rows);overhead=self.c['memory_overhead_mib']
   if type(overhead)!=int or overhead<1:raise ValueError('EXPLICIT_VM_OVERHEAD_REQUIRED')
  command=self.c['guest_bootstrap_command']
  if not isinstance(command,list) or not command or not all(isinstance(x,str) for x in command):raise ValueError('OPERATOR_BOOTSTRAP_COMMAND_REQUIRED')
  display='-'.join(x for x in (project,purpose,session[:8]) if x)
  cfg={'command':[x.replace('{session_id}',session) for x in command],'template_id':template,'template_version':version,'template_disk_sha256':entry['disk']['sha256'] if pins is None else '','guest_cpus':cpus,'memory_mib':memory,'disk_gib':disk,'architecture':arch,'guest_os':'linux','creator_agent_id':agent,'creator_session_id':creator_session,'worker_session_id':session,'project':project,'purpose':purpose,'task_link':link}
  if repository_profile is not None:cfg['repository_profile']=repository_profile
  if repository_profile_sha256 is not None:cfg['repository_profile_sha256']=repository_profile_sha256
  if pins is not None:
   cfg.update(architecture='',template_catalog_sha256=selected_catalog,template_variants_json=canonical(pins).decode())
  meta={'nomad_workers_owner':self.c['owner_id'],'creator_agent_id':agent,'creator_session_id':creator_session,'worker_session_id':session,'task_link':link or 'unknown','template_id':template,'template_version':version,'template_disk_sha256':entry['disk']['sha256'] if pins is None else '','purpose':purpose,'project':project}
  meta.update(described);meta['source_native_session_id']=native.get('native_session_id','');meta['source_native_session_status']='observed'if native.get('native_session_id')else'not_observed';meta['source_identity_evidence']='descriptive_metadata_not_identity_or_liveness';meta['task_link']=link
  meta['creator_identity_evidence']='native_hook_observation_not_authenticated_liveness' if generation else 'operator_declared_unconfirmed'
  if generation:meta['creator_generation']=generation
  jobid='worker-'+session
  if pins is None:
   constraints=[{'LTarget':'${attr.cpu.arch}','Operand':'=','RTarget':arch},{'LTarget':'${attr.kernel.name}','Operand':'=','RTarget':'darwin' if entry['vm_type']=='vz' else 'linux'},{'LTarget':'${attr.driver.lima.cpu.nomad_compute_per_core}','Operand':'<=','RTarget':str(per)}]
  else:
   constraints=[{'LTarget':'${attr.driver.lima.catalog.late_platform_selection}','Operand':'=','RTarget':'true'},{'LTarget':'${attr.driver.lima.catalog.sha256}','Operand':'=','RTarget':selected_catalog},{'LTarget':'${node.unique.id}','Operand':'set_contains_any','RTarget':','.join(placement_plan['candidate_node_ids'])},{'LTarget':'${attr.driver.lima.cpu.nomad_compute_per_core}','Operand':'<=','RTarget':str(per)}]
   meta.update(requested_architecture=arch or'auto',template_catalog_sha256=selected_catalog,template_variants_json=canonical(pins).decode())
   if catalog_route:meta['catalog_source']=catalog_route['catalog_source']
  if repository_profile is not None:
   constraints.append({'LTarget':'${attr.driver.lima.repositories.profile.'+repository_profile+'}','Operand':'='if repository_profile_sha256 else'regexp','RTarget':repository_profile_sha256 or'^[a-f0-9]{64}$'})
   meta['repository_profile_requested']=repository_profile
  if guest_config is not None:meta['coordinator_model']=meta['runtime_coordinator_model']=guest_config['prime_vault']['provider']['model']
  job={'ID':jobid,'Name':display,'Namespace':self.c['namespace'],'Type':'batch','Datacenters':self.c['datacenters'],'Meta':meta,'Constraints':constraints,'TaskGroups':[{'Name':'worker','Count':1,'RestartPolicy':{'Attempts':0,'Mode':'fail'},'ReschedulePolicy':{'Attempts':0,'Unlimited':False},'Tasks':[{'Name':self.c['task'],'Driver':'lima','Config':cfg,'Resources':{'CPU':cpus*per,'MemoryMB':memory+overhead},'LogConfig':{'MaxFiles':10,'MaxFileSizeMB':10}}]}]}
  from disconnect_policy import group_strategy
  job['TaskGroups'][0]['Disconnect']=group_strategy(self.c)
  vault=self.c.get('vault')
  if vault is not None:
   if vault!={'role':'agent-worker-prime'}:raise ValueError('APPROVED_VAULT_ROLE_REQUIRED')
   if not self.c.get('guest',{}).get('prime_vault'):raise ValueError('PRIME_VAULT_RUNTIME_CONFIG_REQUIRED')
   cfg['vault_required']=True
   job['TaskGroups'][0]['Tasks'][0]['Vault']={'Role':vault['role'],'Env':False,'DisableFile':False,'ChangeMode':'noop'}

  # Persist before network. A repeated request is lookup only, never a resubmit.
  root=self.root/session;root.mkdir(exist_ok=True,mode=0o700);p=root/'allocation-intent.json';fingerprint=hashlib.sha256(canonical({'config':cfg,'namespace':self.c['namespace']})).hexdigest()
  if p.exists():
   old=json.loads(p.read_text())
   if old['request_hash']!=fingerprint:raise ValueError('SESSION_REQUEST_CHANGED')
   return self.request_status(session)
  intent={'phase':'submit_intent','job':jobid,'session_id':session,'request_hash':fingerprint,'input_hash':input_hash,'display_name':display,'cpu_reservation_mhz':cpus*per,'memory_reservation_mib':memory+overhead,'prompt_retry':False,'creator_agent_id':agent,'creator_session_id':creator_session,'creator_generation':generation,'creator_record':str(creator_record) if creator_record else None}
  intent['descriptive_provenance']=described
  intent.update(project=project,purpose=purpose)
  if guest_config is not None:intent['guest_config']=guest_config
  if pins is not None:intent.update(placement={'catalog_sha256':selected_catalog,'variants':pins},reservation_envelope=placement_plan)
  if catalog_route:intent['catalog_route']=catalog_route
  intent.update(job_spec=job,job_spec_sha256=hashlib.sha256(canonical(job)).hexdigest(),registration_stage='prepared_not_dispatched')
  atomic(p,intent)
  from allocation_retry import start_dispatch
  start_dispatch(self,p,intent)
  result=self.run(['job','run','-check-index=0','-detach','-json','-'],input=canonical({'Job':job}))
  intent['phase']='submitted' if result.returncode==0 else 'submit_unknown';atomic(p,intent)
  if result.returncode:raise RuntimeError('JOB_SUBMIT_OUTCOME_UNKNOWN_QUERY_SAME_SESSION_NO_RESUBMIT')
  return self.request_status(session)
 def registration_status(self,session,allow_register=False):
  import allocation_retry,fcntl
  root=self.root/uid(session)
  if not allow_register:return allocation_retry.reconcile(self,session,False)
  fd=os.open(root/'request.lock',os.O_RDWR|os.O_CREAT|os.O_NOFOLLOW,0o600)
  with os.fdopen(fd,'a')as lock:
   import stat
   st=os.fstat(lock.fileno())
   if not stat.S_ISREG(st.st_mode)or st.st_uid!=os.getuid()or st.st_mode&0o077:raise RuntimeError('ALLOCATION_PRIVATE_LOCK_REQUIRED')
   fcntl.flock(lock,fcntl.LOCK_EX)
   return allocation_retry.reconcile(self,session,allow_register)
 def recover_copy(self,session,identifier,creator_record,cpus,memory):
  import fcntl,exchange,private_copy
  root=self.root/uid(session);root.mkdir(exist_ok=True,mode=0o700)
  with open(root/'request.lock','a')as lock:
   os.chmod(lock.name,0o600);fcntl.flock(lock,fcntl.LOCK_EX)
   m=private_copy.load(self.c);native=exchange.protected(creator_record)
   fingerprint=hashlib.sha256(canonical({'manifest':self.c['private_copies_sha256'],'id':identifier,'session':session,'creator':{k:native.get(k)for k in ('agent_instance_id','native_session_id','generation')},'cpus':cpus,'memory':memory,'config':self.c})).hexdigest()
   p=root/'allocation-intent.json'
   if p.exists():
    old=json.loads(p.read_text())
    if old.get('private_copy_request_hash')!=fingerprint:raise ValueError('PRIVATE_COPY_REQUEST_CHANGED')
    return self.request_status(session)
   job=private_copy.plan(self.c,m,identifier,session,native,cpus,memory,self.capacity()['nodes'])
   meta=job['Meta'];v=m['inputs'][identifier]
   intent={'phase':'submit_intent','job':job['ID'],'session_id':session,'private_copy_request_hash':fingerprint,'private_copy_id':identifier,'prompt_retry':False,'creator_agent_id':meta['creator_agent_id'],'creator_session_id':meta['creator_session_id'],'creator_generation':meta['creator_generation'],'creator_record':str(creator_record),'recovery_parent':{'source_allocation':v['source_allocation'],'source_session':v['source_session'],'source_key':v['source_key'],'copy_sha256':v['copy_sha256'],'original_retained':True}}
   atomic(p,intent)
   r=self.run(['job','run','-detach','-json','-'],input=canonical({'Job':job}))
   intent['phase']='submitted'if r.returncode==0 else'submit_unknown';atomic(p,intent)
   if r.returncode:raise RuntimeError('JOB_SUBMIT_OUTCOME_UNKNOWN_QUERY_SAME_SESSION_NO_RESUBMIT')
   return self.request_status(session)
 def find_workers(self,project,creator_record,all_owned=False):
  import discovery
  return discovery.find(self,project,creator_record,all_owned)
 def index_lineage(self,session):
  import discovery
  return discovery.index(self,session)
 def resume(self,session,allocation,creator_record):
  import fcntl,resume
  uid(session);uid(allocation)
  if not creator_record:raise ValueError('FRESH_CREATOR_RECORD_REQUIRED')
  root=self.root/session;root.mkdir(exist_ok=True,mode=0o700)
  with open(root/'request.lock','a') as lock:
   os.chmod(lock.name,0o600);fcntl.flock(lock,fcntl.LOCK_EX)
   p=root/'allocation-intent.json'
   if p.exists():
    prior=json.loads(p.read_text())
    if prior.get('resumed_from')!=allocation or prior.get('creator_record')!=str(creator_record):raise ValueError('SESSION_RESUME_SOURCE_CHANGED')
    return self.request_status(session)
   source=self.api('/v1/allocation/'+allocation)
   if source.get('ID')!=allocation:raise ValueError('SOURCE_ALLOCATION_CHANGED')
   import exchange
   native=exchange.protected(creator_record)
   if native.get('ended') or native.get('evidence')!='native_hook_observation_not_authenticated_liveness':raise ValueError('INITIATOR_RECORD_NOT_ACTIVE')
   uid(native['agent_instance_id']);uid(native['generation'])
   creator_session=native['native_session_id']
   if not re.fullmatch('[A-Za-z0-9][A-Za-z0-9._:-]{0,255}',creator_session):raise ValueError('CREATOR_SESSION_ID_INVALID')
   settings={**self.c,'agent_instance_id':native['agent_instance_id'],'creator_session_id':creator_session}
   job=resume.plan(settings,source,session)
   import provenance
   job['Meta'].update(provenance.resumed_descriptions(self.c,native,source.get('Job',{}).get('Meta',{})))
   source_meta=source.get('Job',{}).get('Meta',{})
   source_model=source_meta.get('runtime_coordinator_model')or source_meta.get('coordinator_model')
   guest_config=self.c.get('guest')
   if self.c.get('model_catalog_file'):
    import model_catalog
    guest_config=model_catalog.select_guest(self.c,source_model)
   if source_model and not guest_config:raise ValueError('SOURCE_RUNTIME_CONFIGURATION_REQUIRED')
   if guest_config and isinstance(guest_config.get('prime_vault',{}).get('provider'),dict):
    selected_model=guest_config['prime_vault']['provider'].get('model')
    if source_model and source_model!=selected_model:raise ValueError('SOURCE_COORDINATOR_MODEL_REQUIRES_APPROVED_CONFIGURATION')
    if selected_model:job['Meta'].update(runtime_coordinator_model=selected_model,coordinator_model=selected_model)
   job['Meta']['creator_generation']=native['generation']
   job['Meta']['creator_identity_evidence']='native_hook_observation_not_authenticated_liveness'
   fingerprint=hashlib.sha256(canonical(job)).hexdigest()
   intent={'phase':'submit_intent','job':job['ID'],'session_id':session,'request_hash':fingerprint,'resumed_from':allocation,'prompt_retry':False,'creator_agent_id':native['agent_instance_id'],'creator_session_id':creator_session,'creator_generation':native['generation'],'creator_record':str(creator_record)}
   previous_tasks=[t for g in source['Job']['TaskGroups']for t in g['Tasks']if t['Name']==self.c['task']and t.get('Driver')=='lima']
   intent['resumed_from_session']=uid(previous_tasks[0]['Config']['worker_session_id'])
   # Only runtime configuration is inherited; no old ACP/request/session state.
   if guest_config:intent['guest_config']=guest_config
   cfg=job['TaskGroups'][0]['Tasks'][0]['Config']
   if cfg.get('template_variants_json'):intent['placement']={'catalog_sha256':cfg['template_catalog_sha256'],'variants':json.loads(cfg['template_variants_json'])}
   atomic(p,intent)
   result=self.run(['job','run','-detach','-json','-'],input=canonical({'Job':job}))
   intent['phase']='submitted' if result.returncode==0 else 'submit_unknown';atomic(p,intent)
   if result.returncode:raise RuntimeError('RESUME_SUBMIT_OUTCOME_UNKNOWN_QUERY_SAME_SESSION_NO_RESUBMIT')
   return self.request_status(session)
 def request_status(self,session):
  uid(session);root=self.root/session;p=root/'allocation-intent.json';intent=json.loads(p.read_text());jobid=intent['job'];ns=self.c['namespace']
  import urllib.parse
  rows=self.api('/v1/job/'+jobid+'/allocations?namespace='+urllib.parse.quote(ns,safe=''))
  guest=intent.get('guest_config',self.c.get('guest',{}));argv=guest.get('agent_argv',[])
  runtime={'name':self.c.get('runtime_name')or(pathlib.Path(argv[0]).name if argv and isinstance(argv[0],str)else None),'selection_evidence':'configured_command','readiness_observed':False}
  active=[a for a in rows if a.get('ClientStatus') not in ('complete','failed','lost')]
  if len(active)>1:raise RuntimeError('MULTIPLE_ALLOCATIONS_REFUSE_BINDING')
  if not active:return {**intent,'allocation':None,'allocations':[{'id':a['ID'],'status':a.get('ClientStatus')} for a in rows],'vm_ready':None,'runtime_ready':None,'session_ready':None,'transport_ready':None,'runtime':runtime,'readiness_stage':'allocation_terminal'if rows else'awaiting_allocation','next_action':'inspect_terminal_allocation'if rows else'request-status'}
  a=active[0];full=self.api('/v1/allocation/'+a['ID']);meta=full.get('Job',{}).get('Meta',{})
  if meta.get('nomad_workers_owner')!=self.c['owner_id'] or meta.get('creator_agent_id')!=intent.get('creator_agent_id',self.c['agent_instance_id']) or meta.get('creator_session_id')!=intent.get('creator_session_id',session):raise RuntimeError('ALLOCATION_OWNER_MISMATCH')
  if intent.get('creator_generation') and (meta.get('creator_generation')!=intent['creator_generation'] or meta.get('worker_session_id')!=session):raise RuntimeError('ALLOCATION_OWNER_GENERATION_OR_SESSION_MISMATCH')
  resolved=None
  if intent.get('placement'):
   import placement
   resolved=placement.selected(intent['placement'],full,self.c['task'])
   if resolved is None or full.get('ClientStatus')!='running' or full.get('TaskStates',{}).get(self.c['task'],{}).get('State')!='running' or not full.get('TaskStates',{}).get(self.c['task'],{}).get('StartedAt'):return {**intent,'allocation':a['ID'],'client_status':a.get('ClientStatus'),'target_config':None,'vm_ready':None,'runtime_ready':None,'session_ready':None,'transport_ready':None,'runtime':runtime,'readiness':'waiting for durable driver platform selection','readiness_stage':'awaiting_running_platform','next_action':'request-status'}
  binding={**self.c,'allocation':a['ID'],'job':jobid,'diagnostics_required':True,'agent_instance_id':intent.get('creator_agent_id',self.c['agent_instance_id']),'creator_session_id':intent.get('creator_session_id',session),'creator_generation':intent.get('creator_generation'),'creator_record':intent.get('creator_record')}
  if intent.get('catalog_route'):binding['catalog_route']=intent['catalog_route']
  if intent.get('recovery_parent'):binding['recovery_parent']=intent['recovery_parent']
  if intent.get('resumed_from'):binding['resume_parent']={'allocation_id':intent['resumed_from'],'worker_session_id':intent.get('resumed_from_session'),'historical_only':True}
  if intent.get('guest_config') is not None:binding['guest']=intent['guest_config']
  if resolved is not None:binding.update(resolved_template=resolved,placement_attempt=placement.allocation_attempt(full,self.c['task']))
  path=root/'target-config.json'
  if path.exists() and json.loads(path.read_text())!=binding:
   import catalog_routing
   saved=json.loads(path.read_text())
   # Only the optional discovery-source list may differ during rollout. Keep
   # the existing target bytes; allocation, attempt, owner and pins still match.
   without_sources=lambda v:{k:x for k,x in v.items()if k!='catalog_sources'}
   if not catalog_routing.enabled(self.c) or without_sources(saved)!=without_sources(binding):raise RuntimeError('BOUND_ALLOCATION_OR_PLATFORM_CHANGED_NO_REBIND')
   binding=saved
  if not path.exists():atomic(path,binding)
  status={**intent,'allocation':a['ID'],'client_status':a.get('ClientStatus'),'target_config':str(path),'vm_ready':None,'runtime_ready':None,'session_ready':None,'transport_ready':None,'runtime':runtime,'readiness':'transport, runtime and session have not been probed by request-status','readiness_stage':'allocation_pending','next_action':'request-status','prompt_retry':False}
  if intent.get('resumed_from'):status['resume_lineage']={'previous_allocation_id':intent['resumed_from'],'previous_worker_session_id':intent.get('resumed_from_session'),'current_allocation_id':a['ID'],'current_worker_session_id':session,'current_target_config':str(path),'previous_ids_historical_only':True,'automatic_alias_adoption':False}
  task=full.get('TaskStates',{}).get(self.c['task'],{});started=task.get('StartedAt')
  if full.get('ClientStatus')!='running'or task.get('State')!='running'or not started or started.startswith('0001-'):return status
  if intent.get('recovery_parent')or intent.get('resumed_from'):
   import discovery
   try:status['id_lineage']=discovery.index(self,session)
   except (RuntimeError,ValueError,KeyError,OSError):status['id_lineage']={'indexed':False,'state':'verification_pending','automatic_alias_adoption':False}
  local=root/'session.json'
  if not local.exists():
   status.update(vm_ready=True,readiness_stage='session_initialization_required',next_action='create',create_policy='call create once; inspect unknown outcome without retry',next_command=['nomad-workers','--config',str(path),'create','--session',session]);return status
  if local.is_symlink():raise RuntimeError('UNSAFE_LOCAL_SESSION_NO_RECREATE')
  current=json.loads(local.read_text());attempt={'allocation':a['ID'],'job':jobid,'task':self.c['task'],'job_version':full.get('Job',{}).get('Version'),'restarts':task.get('Restarts',0),'started_at':started}
  if binding.get('resolved_template'):attempt['resolved_template']=binding['resolved_template']
  if current.get('config_hash')!=hashlib.sha256(canonical(binding)).hexdigest()or current.get('attempt')!=attempt:raise RuntimeError('BOUND_SESSION_ATTEMPT_CHANGED_NO_RECREATE')
  phase=current.get('phase');status['session_phase']=phase;status['vm_ready']=True
  if phase=='ready'and current.get('record_id'):
   status.update(readiness_stage='session_ready_receipt',record_id=current['record_id'],last_observed_session_ready=True,next_action='status',readiness='durable session-create receipt is ready; current transport/runtime/session liveness was not probed')
  else:status.update(readiness_stage='session_initialization_unknown'if phase in ('create_intent','create_unknown')else'session_not_ready',next_action='inspect_existing_session_no_create_retry',readiness='existing session intent must not be recreated')
  return status
 def propose(self,reason,tools):
  if not reason or len(reason)>4096 or len(tools)>64:raise ValueError('bounded reason/tools required')
  proposal={'id':str(uuid.uuid4()),'reason':reason,'tools':tools,'state':'operator_review_required','published':False};atomic(self.root/('environment-proposal-'+proposal['id']+'.json'),proposal);return proposal
