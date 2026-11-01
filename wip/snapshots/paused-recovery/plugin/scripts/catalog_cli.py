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
 def __init__(self,config):
  self.config=pathlib.Path(config);self.c=json.loads(self.config.read_text());self.root=pathlib.Path(self.c['receipt_root']);self.root.mkdir(parents=True,exist_ok=True,mode=0o700)
  if self.root.is_symlink() or self.root.stat().st_mode&0o077:raise ValueError('private receipt_root required')
  self.bin=self.c.get('nomad_bin','nomad')
 def run(self,args,input=None):return subprocess.run([self.bin,*args],input=input,capture_output=True)
 def api(self,path):
  r=self.run(['operator','api','-X','GET',path])
  if r.returncode:raise RuntimeError('NOMAD_READ_UNAVAILABLE_OR_ACL_DENIED')
  try:return json.loads(r.stdout)
  except (ValueError,UnicodeDecodeError):raise RuntimeError('NOMAD_READ_INVALID_JSON') from None
 def templates(self):
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
   out.append({'node_status':n.get('Status'),'scheduling_eligibility':n.get('SchedulingEligibility'),'driver':{'name':'lima','detected':bool(drv.get('Detected')),'healthy':bool(drv.get('Healthy'))},'placement_eligible':not reasons,'placement_reasons':reasons,'late_platform_selection':value('driver.lima.catalog.late_platform_selection') in (True,'true'),'catalog_sha256':value('driver.lima.catalog.sha256'),'available_for_one_new_vm':effective,'occupied':{'cpu_mhz':used_cpu,'reservation_mib':used_mem,'driver':telemetry},'node_id':d['ID'],'name':d.get('Name'),'architecture':attrs.get('cpu.arch'),'host_os':attrs.get('kernel.name'),'cpu_mhz_per_vcpu':per,'allocatable_cpu_mhz':max(0,cpu),'used_cpu_mhz':used_cpu,'free_cpu_mhz':max(0,cpu-used_cpu),'free_guest_vcpus_by_cpu':max(0,cpu-used_cpu)//per if per>0 and not reasons else None,'memory_overhead_mib_per_vm':value('driver.lima.operator_limits.memory_overhead_mib'),'free_guest_memory_mib_for_one_vm':None if reasons else max(0,mem-used_mem-int(value('driver.lima.operator_limits.memory_overhead_mib') or self.c.get('memory_overhead_mib',0))),'allocatable_reservation_mib':max(0,mem),'used_reservation_mib':used_mem,'free_reservation_mib':max(0,mem-used_mem),'disk_free':'unknown_until_authoritative_driver_admission','node_operator_limits':{k:value('driver.lima.operator_limits.'+k) for k in ('max_vms','max_total_vcpus','max_total_memory_mib','max_total_disk_gib','max_guest_vcpus','max_guest_memory_mib','max_guest_disk_gib','memory_overhead_mib')},'advisory':True})
  return {'nodes':out,'resource_scope':'per_node_not_interchangeable_or_summed','operator_limits':self.c.get('request_limits',{}),'quota_authority':'Nomad reservations and configured driver node budgets; displayed free capacity is advisory','auto_close_enabled':False}
 def request_templates(self):
  entries=self.templates()
  if self.c.get('model_catalog_file'):
   import model_catalog
   model_catalog.load(self.c)
   entries=[e for e in entries if e.get('tools',{}).get('prime_model_catalog')=='1']
   if not entries:raise ValueError('MODEL_CATALOG_IMAGE_REQUIRED')
  return entries
 def capacity_request(self,template,version,arch,cpus,memory,disk):
  import placement
  if any(type(v)!=int or v<1 for v in (cpus,memory,disk)):raise ValueError('POSITIVE_REQUEST_RESOURCES_REQUIRED')
  arch=None if arch in (None,'auto') else arch
  if arch not in (None,'amd64','arm64'):raise ValueError('UNSUPPORTED_REQUEST_ARCHITECTURE')
  result=self.capacity();pins=placement.variants(self.request_templates(),template,version,arch,disk)
  result['auto_placement_request_fit']=placement.envelope(pins,result['nodes'],cpus,memory,disk,self.c['catalog_sha256'])
  return result
 def request(self,session,template,version,arch,cpus,memory,disk,purpose,project='',link='',creator_record=None,model=None):
  import fcntl
  root=self.root/uid(session);root.mkdir(exist_ok=True,mode=0o700)
  with open(root/'request.lock','a') as lock:
   os.chmod(lock.name,0o600);fcntl.flock(lock,fcntl.LOCK_EX)
   return self._request_locked(session,template,version,arch,cpus,memory,disk,purpose,project,link,creator_record,model)
 def _request_locked(self,session,template,version,arch,cpus,memory,disk,purpose,project='',link='',creator_record=None,model=None):
  uid(session);uid(self.c['owner_id']);uid(self.c['agent_instance_id'])
  creator_session=self.c.get('creator_session_id',session);agent=self.c['agent_instance_id'];generation=None
  if creator_record:
   import exchange
   native=exchange.protected(creator_record)
   if native.get('ended') or native.get('evidence')!='native_hook_observation_not_authenticated_liveness':raise ValueError('INITIATOR_RECORD_NOT_ACTIVE')
   uid(native['agent_instance_id']);uid(native['generation']);creator_session=native['native_session_id'];agent=native['agent_instance_id'];generation=native['generation']
  if not re.fullmatch('[A-Za-z0-9][A-Za-z0-9._:-]{0,255}',creator_session):raise ValueError('CREATOR_SESSION_ID_INVALID')
  if not re.fullmatch('[a-z0-9][a-z0-9-]{0,31}',purpose) or (project and not re.fullmatch('[a-z0-9][a-z0-9-]{0,31}',project)):raise ValueError('project/purpose slug required')
  if len(link)>1024 or any(c in link for c in '\r\n\x00'):raise ValueError('invalid task link')
  if any(type(x)!=int or x<1 for x in (cpus,memory,disk)):raise ValueError('positive integer resources required')
  limits=self.c.get('request_limits',{})
  for k,n in [('vcpus',cpus),('memory_mib',memory),('disk_gib',disk)]:
   if limits.get(k,0)>0 and n>limits[k]:raise ValueError('REQUEST_EXCEEDS_OPERATOR_POLICY')
  arch=None if arch in (None,'auto') else arch
  if arch not in (None,'amd64','arm64'):raise ValueError('UNSUPPORTED_REQUEST_ARCHITECTURE')
  root=self.root/session;p=root/'allocation-intent.json'
  input_hash=hashlib.sha256(canonical({'config':self.c,'session':session,'template':template,'version':version,'architecture':arch,'vcpus':cpus,'memory_mib':memory,'disk_gib':disk,'purpose':purpose,'project':project,'link':link,'creator_agent':agent,'creator_session':creator_session,'creator_generation':generation,'coordinator_model':model})).hexdigest()
  if p.exists():
   previous=json.loads(p.read_text())
   if 'input_hash' in previous:
    if previous['input_hash']!=input_hash:raise ValueError('SESSION_REQUEST_CHANGED')
    return self.request_status(session)
  guest_config=None
  if self.c.get('model_catalog_file') or model is not None:
   import model_catalog
   guest_config=model_catalog.select_guest(self.c,model)
  approved=self.request_templates()
  placement_plan=None;pins=None
  if arch is None:
   import placement
   pins=placement.variants(approved,template,version,None,disk)
   placement_plan=placement.envelope(pins,self.capacity()['nodes'],cpus,memory,disk,self.c['catalog_sha256'])
   per=placement_plan['compute_per_core'];overhead=placement_plan['overhead_mib']
  else:
   entries=[e for e in approved if (e['template_id'],e['version'],e['architecture'])==(template,version,arch)]
   if not entries:raise ValueError('APPROVED_TEMPLATE_UNAVAILABLE')
   if len(entries)!=1:raise ValueError('EXPLICIT_PLATFORM_REQUIRED_FOR_AMBIGUOUS_TEMPLATE')
   entry=entries[0];acc=entry.get('acceptance',{})
   if not acc.get('sealed') or acc.get('independent_boots',0)<2 or not acc.get('unique_identities'):raise ValueError('TEMPLATE_ACCEPTANCE_PENDING')
   if disk<entry['disk']['minimum_disk_gib']:raise ValueError('DISK_BELOW_IMAGE_MINIMUM')
   rows=[n for n in self.capacity()['nodes'] if n.get('placement_eligible',True) and n.get('architecture')==arch and n.get('host_os')==('darwin' if entry['vm_type']=='vz' else 'linux') and n.get('cpu_mhz_per_vcpu',0)>0]
   if not rows:raise RuntimeError('ELIGIBLE_NODE_INVENTORY_UNAVAILABLE')
   per=max(n['cpu_mhz_per_vcpu'] for n in rows);overhead=self.c['memory_overhead_mib']
   if type(overhead)!=int or overhead<1:raise ValueError('EXPLICIT_VM_OVERHEAD_REQUIRED')
  command=self.c['guest_bootstrap_command']
  if not isinstance(command,list) or not command or not all(isinstance(x,str) for x in command):raise ValueError('OPERATOR_BOOTSTRAP_COMMAND_REQUIRED')
  display='-'.join(x for x in (project,purpose,session[:8]) if x)
  cfg={'command':[x.replace('{session_id}',session) for x in command],'template_id':template,'template_version':version,'template_disk_sha256':entry['disk']['sha256'] if pins is None else '','guest_cpus':cpus,'memory_mib':memory,'disk_gib':disk,'architecture':arch,'guest_os':'linux','creator_agent_id':agent,'creator_session_id':creator_session,'worker_session_id':session,'project':project,'purpose':purpose,'task_link':link}
  if pins is not None:
   cfg.update(architecture='',template_catalog_sha256=self.c['catalog_sha256'],template_variants_json=canonical(pins).decode())
  meta={'nomad_workers_owner':self.c['owner_id'],'creator_agent_id':agent,'creator_session_id':creator_session,'worker_session_id':session,'task_link':link or 'unknown','template_id':template,'template_version':version,'template_disk_sha256':entry['disk']['sha256'] if pins is None else '','purpose':purpose,'project':project}
  meta['creator_identity_evidence']='native_hook_observation_not_authenticated_liveness' if generation else 'operator_declared_unconfirmed'
  if generation:meta['creator_generation']=generation
  jobid='worker-'+session
  if pins is None:
   constraints=[{'LTarget':'${attr.cpu.arch}','Operand':'=','RTarget':arch},{'LTarget':'${attr.kernel.name}','Operand':'=','RTarget':'darwin' if entry['vm_type']=='vz' else 'linux'},{'LTarget':'${attr.driver.lima.cpu.nomad_compute_per_core}','Operand':'<=','RTarget':str(per)}]
  else:
   constraints=[{'LTarget':'${attr.driver.lima.catalog.late_platform_selection}','Operand':'=','RTarget':'true'},{'LTarget':'${attr.driver.lima.catalog.sha256}','Operand':'=','RTarget':self.c['catalog_sha256']},{'LTarget':'${node.unique.id}','Operand':'set_contains_any','RTarget':','.join(placement_plan['candidate_node_ids'])},{'LTarget':'${attr.driver.lima.cpu.nomad_compute_per_core}','Operand':'<=','RTarget':str(per)}]
   meta.update(requested_architecture='auto',template_catalog_sha256=self.c['catalog_sha256'],template_variants_json=canonical(pins).decode())
  if guest_config is not None:meta['coordinator_model']=guest_config['prime_vault']['provider']['model']
  job={'ID':jobid,'Name':display,'Namespace':self.c['namespace'],'Type':'batch','Datacenters':self.c['datacenters'],'Meta':meta,'Constraints':constraints,'TaskGroups':[{'Name':'worker','Count':1,'RestartPolicy':{'Attempts':0,'Mode':'fail'},'ReschedulePolicy':{'Attempts':0,'Unlimited':False},'Tasks':[{'Name':self.c['task'],'Driver':'lima','Config':cfg,'Resources':{'CPU':cpus*per,'MemoryMB':memory+overhead},'LogConfig':{'MaxFiles':10,'MaxFileSizeMB':10}}]}]}
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
  if guest_config is not None:intent['guest_config']=guest_config
  if pins is not None:intent.update(placement={'catalog_sha256':self.c['catalog_sha256'],'variants':pins},reservation_envelope=placement_plan)
  atomic(p,intent)
  result=self.run(['job','run','-detach','-json','-'],input=canonical({'Job':job}))
  intent['phase']='submitted' if result.returncode==0 else 'submit_unknown';atomic(p,intent)
  if result.returncode:raise RuntimeError('JOB_SUBMIT_OUTCOME_UNKNOWN_QUERY_SAME_SESSION_NO_RESUBMIT')
  return self.request_status(session)
 def request_status(self,session):
  uid(session);root=self.root/session;p=root/'allocation-intent.json';intent=json.loads(p.read_text());jobid=intent['job'];ns=self.c['namespace']
  import urllib.parse
  rows=self.api('/v1/job/'+jobid+'/allocations?namespace='+urllib.parse.quote(ns,safe=''))
  active=[a for a in rows if a.get('ClientStatus') not in ('complete','failed','lost')]
  if len(active)>1:raise RuntimeError('MULTIPLE_ALLOCATIONS_REFUSE_BINDING')
  if not active:return {**intent,'allocation':None,'allocations':[{'id':a['ID'],'status':a.get('ClientStatus')} for a in rows]}
  a=active[0];full=self.api('/v1/allocation/'+a['ID']);meta=full.get('Job',{}).get('Meta',{})
  if meta.get('nomad_workers_owner')!=self.c['owner_id'] or meta.get('creator_agent_id')!=intent.get('creator_agent_id',self.c['agent_instance_id']) or meta.get('creator_session_id')!=intent.get('creator_session_id',session):raise RuntimeError('ALLOCATION_OWNER_MISMATCH')
  resolved=None
  if intent.get('placement'):
   import placement
   resolved=placement.selected(intent['placement'],full,self.c['task'])
   if resolved is None or full.get('ClientStatus')!='running' or full.get('TaskStates',{}).get(self.c['task'],{}).get('State')!='running' or not full.get('TaskStates',{}).get(self.c['task'],{}).get('StartedAt'):return {**intent,'allocation':a['ID'],'client_status':a.get('ClientStatus'),'target_config':None,'prime_ready':False,'readiness':'waiting for durable driver platform selection'}
  binding={**self.c,'allocation':a['ID'],'job':jobid,'diagnostics_required':True,'agent_instance_id':intent.get('creator_agent_id',self.c['agent_instance_id']),'creator_session_id':intent.get('creator_session_id',session),'creator_generation':intent.get('creator_generation'),'creator_record':intent.get('creator_record')}
  if intent.get('guest_config') is not None:binding['guest']=intent['guest_config']
  if resolved is not None:binding.update(resolved_template=resolved,placement_attempt=placement.allocation_attempt(full,self.c['task']))
  path=root/'target-config.json'
  if path.exists() and json.loads(path.read_text())!=binding:raise RuntimeError('BOUND_ALLOCATION_OR_PLATFORM_CHANGED_NO_REBIND')
  if not path.exists():atomic(path,binding)
  return {**intent,'allocation':a['ID'],'client_status':a.get('ClientStatus'),'target_config':str(path),'prime_ready':False,'readiness':'allocation status is not ACP readiness'}
 def propose(self,reason,tools):
  if not reason or len(reason)>4096 or len(tools)>64:raise ValueError('bounded reason/tools required')
  proposal={'id':str(uuid.uuid4()),'reason':reason,'tools':tools,'state':'operator_review_required','published':False};atomic(self.root/('environment-proposal-'+proposal['id']+'.json'),proposal);return proposal
