#!/usr/bin/env python3
"""Candidate-only sanitation, invoked inside a separately verified clone.
Never run on a source VM. Secret detection is finite pattern coverage, not a
universal guarantee. No secret values or file contents are returned in receipts.
"""
import hashlib,json,os,pathlib,re,shutil,stat,sys
sys.path.insert(0,str(pathlib.Path(__file__).parents[1]/'plugin/scripts'))
import template_save as save
RUNTIME=('/var/tmp/nomad-workers','/opt/worker/state','/root/.prime','/root/.acpx','/root/.config/prime','/root/.bao-token','/root/.vault-token','/root/.ssh')
SECRET=re.compile(rb'-----BEGIN (?:RSA |EC |OPENSSH |DSA )?PRIVATE KEY-----|(?<![A-Za-z0-9_])(?:hvs|hvb|s)\.[A-Za-z0-9_-]{16,}(?![A-Za-z0-9_-])|(?i:(?:LLM_API_KEY|VAULT_TOKEN|BAO_TOKEN|NOMAD_TOKEN|AWS_SECRET_ACCESS_KEY|api[_-]?key|client[_-]?secret|password)\s*[=:]\s*["\']?)(?!["\']?(?:\$|\{|null\b|None\b|false\b|["\']?[ \t\r\n]))[^\s"\']{12,}')
class SanitationError(RuntimeError):pass

def location(root,guest):
 p=pathlib.PurePosixPath(guest)
 if not p.is_absolute() or '..' in p.parts or str(p)=='/':raise SanitationError('INVALID_CANDIDATE_PATH')
 result=root.joinpath(*p.parts[1:]);current=root
 for part in p.parts[1:-1]:
  current=current/part
  if current.is_symlink():raise SanitationError('CANDIDATE_PARENT_SYMLINK_REJECTED')
 return result

CLEAN_CONTENT_ROOTS=('/tmp','/var/tmp','/var/cache/apt/archives')
SCAN_CHUNK_BYTES=1048576
def removable(guest):
 return guest in CLEAN_CONTENT_ROOTS or guest in RUNTIME or any(guest.startswith(prefix) for prefix in ('/root/','/home/','/workspace/','/var/tmp/','/opt/worker/state/'))

def provenance_contract(packet,evidence):
 # Evidence is supplied only by the trusted operator binding, pinned by its
 # private context SHA. It is not an agent-provided per-directory waiver.
 if evidence is None:return {}
 if not isinstance(evidence,dict) or evidence.get('schema_version')!=1 or evidence.get('operation_id')!=packet['plan']['operation_id'] or evidence.get('plan_sha256')!=packet['plan_sha256']:raise SanitationError('TRUSTED_ARTIFACT_PROVENANCE_BINDING_REQUIRED')
 files=evidence.get('files');result={}
 if not isinstance(files,list)or len(files)>10000:raise SanitationError('TRUSTED_ARTIFACT_PROVENANCE_INVALID')
 for item in files:
  path=item.get('path','');proof=item.get('provenance',{})
  if not isinstance(path,str)or not (path.startswith('/usr/')or path.startswith('/root/.cache/uv/archive-v0/'))or '..'in pathlib.PurePosixPath(path).parts or path in result:raise SanitationError('TRUSTED_ARTIFACT_PATH_REJECTED')
  if not re.fullmatch('[a-f0-9]{64}',item.get('sha256',''))or proof.get('kind')not in ('accepted_base_disk','official_archive','installed_package_checksum','reproduced_package_metadata')or not re.fullmatch('[a-f0-9]{64}',proof.get('evidence_sha256','')):raise SanitationError('TRUSTED_ARTIFACT_PROVENANCE_INVALID')
  result[path]=item['sha256']
 return result

def scrub(root,packet,clone_receipt,marker,max_bytes=68719476736,max_file_bytes=None,trusted_provenance=None):
 root=pathlib.Path(root)
 if root.is_symlink() or not root.is_dir():raise SanitationError('CANDIDATE_ROOT_UNSAFE')
 p=packet['plan'];composition=save.composition_contract(p.get('data_composition'))
 if save.sha(p)!=packet['plan_sha256'] or p.get('method')!='official_stopped_disk_clone_then_existing_seal_export_pipeline':raise SanitationError('BOUND_CLONE_PLAN_REQUIRED')
 save.verify_stage('clone_candidate',clone_receipt,packet)
 trusted=provenance_contract(packet,trusted_provenance)
 expected={'operation_id':p['operation_id'],'plan_sha256':packet['plan_sha256'],'candidate_instance':clone_receipt.get('candidate_instance'),'source_allocation':p['source']['allocation'],'purpose':'candidate-only-sanitation'}
 if not expected['candidate_instance'] or marker!=expected:raise SanitationError('EXACT_CANDIDATE_MARKER_REQUIRED')
 if any(not removable(path) for path in composition['remove']):raise SanitationError('REMOVAL_OUTSIDE_REVIEWABLE_DATA_PATHS')
 # Caller must not retain application runtime history, even implicitly via a
 # retained parent. Refuse contradictory composition instead of silently loss.
 runtime=list(RUNTIME)
 homes=root/'home'
 if homes.exists():
  if homes.is_symlink():raise SanitationError('CANDIDATE_HOME_SYMLINK_REJECTED')
  for user in homes.iterdir():
   if user.is_symlink():raise SanitationError('CANDIDATE_USER_SYMLINK_REJECTED')
   runtime.extend('/home/'+user.name+'/'+name for name in ('.prime','.acpx','.config/prime','.bao-token','.vault-token','.ssh'))
 for keep in composition['retain']:
  if any(keep==r or keep.startswith(r+'/') or r.startswith(keep.rstrip('/')+'/') for r in runtime):raise SanitationError('RUNTIME_STATE_CANNOT_BE_RETAINED')
 # Validate every path before making any change. A leaf symlink is unlinked;
 # symlink parents are never traversed.
 workspace=location(root,'/workspace/repo');workspace_owner=(workspace.stat().st_uid,workspace.stat().st_gid) if workspace.exists() and not workspace.is_symlink() else (1000,1000)
 content_roots={location(root,path)for path in composition['remove']if path in CLEAN_CONTENT_ROOTS}
 for path in content_roots:
  if path.is_symlink()or(path.exists()and not path.is_dir()):raise SanitationError('CANDIDATE_CLEANUP_ROOT_SYMLINK_OR_TYPE')
 targets=[location(root,path) for path in dict.fromkeys(runtime+composition['remove'])]
 ssh=location(root,'/etc/ssh')
 if ssh.is_symlink():raise SanitationError('CANDIDATE_SSH_SYMLINK_REJECTED')
 if ssh.exists():targets.extend(ssh.glob('ssh_host_*_key*'))
 for path in targets:
  if path in content_roots:
   if path.exists():
    for child in path.iterdir():
     if child.is_symlink()or not child.is_dir():child.unlink()
     else:shutil.rmtree(child)
   continue
  if path.is_symlink() or path.is_file():path.unlink()
  elif path.exists():
   if not path.is_dir():raise SanitationError('SPECIAL_DATA_PATH_REJECTED')
   shutil.rmtree(path)
 if not workspace.exists():
  workspace.mkdir(parents=True,mode=0o700);os.chown(workspace,*workspace_owner)
 # Scan preserved user/config data and candidate logs before identity sealing.
 # Bounded streaming scan includes explicitly retained large binaries. No
 # following symlinks or silently skipping libraries. Whitespace normalization
 # preserves unbounded whitespace in credential patterns across chunk edges.
 scanned=total=verified_public_matches=0;inventory=hashlib.sha256()
 roots=['/root','/home','/workspace','/etc','/var/tmp','/tmp','/var/log']+composition['retain']
 seen=set()
 for guest in roots:
  base=location(root,guest)
  if not base.exists() or base.is_symlink():continue
  paths=[base] if base.is_file() else (pathlib.Path(d)/name for d,dirs,files in os.walk(base,followlinks=False) for name in files)
  for path in paths:
   if path in seen or path.is_symlink():continue
   seen.add(path);st=path.stat()
   if not stat.S_ISREG(st.st_mode):continue
   if max_file_bytes is not None and st.st_size>max_file_bytes:raise SanitationError('CANDIDATE_SCAN_FILE_QUOTA_REVIEW_REQUIRED')
   if total+st.st_size>max_bytes:raise SanitationError('CANDIDATE_SCAN_TOTAL_QUOTA_REVIEW_REQUIRED')
   digest=hashlib.sha256();carry=b'';matched=False
   with path.open('rb')as stream:
    while True:
     raw=stream.read(SCAN_CHUNK_BYTES)
     if not raw:break
     total+=len(raw)
     if total>max_bytes:raise SanitationError('CANDIDATE_SCAN_TOTAL_QUOTA_REVIEW_REQUIRED')
     digest.update(raw)
     # SECRET's shortest non-whitespace match is <128B. Compress whitespace
     # runs before keeping a 1KiB suffix, including across successive chunks;
     # a credential name followed by megabytes of whitespace cannot evade it.
     window=re.sub(rb'\s+',b' ',carry+raw)
     if SECRET.search(window):matched=True
     carry=window[-1024:]
   if matched:
    guest_path='/'+path.relative_to(root).as_posix()
    if trusted.get(guest_path)!=digest.hexdigest():raise SanitationError('CANDIDATE_SECRET_PATTERN_FOUND_DATA_RETAINED')
    verified_public_matches+=1
   inventory.update(str(path.relative_to(root)).encode()+b'\0'+digest.digest());scanned+=1
 return {'operation_id':p['operation_id'],'plan_sha256':packet['plan_sha256'],'candidate_instance':clone_receipt['candidate_instance'],'runtime_credentials_absent':True,'approved_data_composition_verified':True,'composition_sha256':save.sha(composition),'scan_scope':'known-runtime-paths plus preserved user/config/log data and explicit retained paths; finite pattern coverage','scan_files':scanned,'verified_public_artifact_match_files':verified_public_matches,'trusted_provenance_sha256':save.sha(trusted_provenance)if trusted_provenance is not None else None,'scan_bytes':total,'scan_inventory_sha256':inventory.hexdigest(),'identity_sealed':False,'candidate_stopped':False,'source_disk_unchanged':clone_receipt['source_disk_unchanged']}

if __name__=='__main__':
 try:
  # The existing trusted host binding verifies the candidate name, no runtime
  # processes and mount policy, then creates this root-owned private tmpfs marker.
  marker_path=pathlib.Path('/run/nomad-workers-template-candidate.json');st=marker_path.lstat()
  if not stat.S_ISREG(st.st_mode) or st.st_uid!=0 or st.st_mode&0o077:raise SanitationError('PRIVATE_CANDIDATE_MARKER_REQUIRED')
  payload=json.load(sys.stdin)
  print(json.dumps(scrub('/',payload['packet'],payload['receipts']['clone_candidate'],json.loads(marker_path.read_text()))))
 except Exception as e:
  print(json.dumps({'error':str(e) if re.fullmatch('[A-Z_]{1,100}',str(e)) else type(e).__name__}),file=sys.stderr);sys.exit(1)
