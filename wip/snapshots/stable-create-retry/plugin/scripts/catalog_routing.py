"""Caller-only routing across separately pinned native catalogs.
One request chooses one existing catalog pin; it never asks drivers to trust a
union catalog or retries submission on another source after an unknown outcome.
"""
import hashlib,json,pathlib,re
import placement

HASH=re.compile('[a-f0-9]{64}')
SOURCE=re.compile('[a-z][a-z0-9-]{0,31}')

def enabled(config):return 'catalog_sources' in config

def metadata(config):
 extras=config.get('catalog_sources')
 if not isinstance(extras,list) or len(extras)>15:raise ValueError('CATALOG_SOURCES_SCHEMA')
 sources=[{'id':'primary','catalog_file':config['catalog_file'],'catalog_sha256':config['catalog_sha256']}]
 ids={'primary'};hashes=set();out=[];identities={}
 for source in extras:
  if not isinstance(source,dict) or set(source)!={'id','catalog_file','catalog_sha256'}:raise ValueError('CATALOG_SOURCE_SCHEMA')
  if not isinstance(source['id'],str) or not SOURCE.fullmatch(source['id']) or source['id'] in ids:raise ValueError('CATALOG_SOURCE_ID')
  ids.add(source['id']);sources.append(source)
 for source in sources:
  sha=source['catalog_sha256'];path=source['catalog_file']
  if not isinstance(sha,str) or not HASH.fullmatch(sha) or sha in hashes:raise ValueError('CATALOG_SOURCE_PIN')
  if not isinstance(path,str) or not pathlib.PurePosixPath(path).is_absolute() or '..' in pathlib.PurePosixPath(path).parts:raise ValueError('CATALOG_SOURCE_PATH')
  hashes.add(sha)
  # Bound every source read, including files which grew after a metadata stat.
  try:
   with pathlib.Path(path).open('rb')as stream:raw=stream.read(4194305)
  except OSError:raise RuntimeError('CATALOG_SOURCE_UNAVAILABLE')from None
  if len(raw)>4194304 or hashlib.sha256(raw).hexdigest()!=sha:raise ValueError('CATALOG_SOURCE_CHECKSUM_MISMATCH')
  doc=json.loads(raw)
  if not isinstance(doc,dict) or doc.get('schema_version')!=1 or not isinstance(doc.get('templates'),list):raise ValueError('CATALOG_SOURCE_VERSION')
  seen=set()
  for entry in doc['templates']:
   if not isinstance(entry,dict) or not isinstance(entry.get('disk'),dict) or not isinstance(entry.get('runtime_yaml'),dict):raise ValueError('CATALOG_SOURCE_ENTRY')
   identity=tuple(entry.get(k)for k in ('template_id','version','architecture','vm_type'))
   if any(not isinstance(v,str)for v in identity) or identity in seen:raise ValueError('CATALOG_SOURCE_DUPLICATE_PLATFORM')
   seen.add(identity)
   # Never expose local paths, URLs, credentials or source-provided route labels.
   safe={k:v for k,v in entry.items()if k in ('template_id','version','architecture','vm_type','guest_os')}
   tools=entry.get('tools',{});acceptance=entry.get('acceptance',{})
   if not isinstance(tools,dict)or not isinstance(acceptance,dict):raise ValueError('CATALOG_SOURCE_METADATA')
   safe['tools']={k:v for k,v in tools.items()if k in ('prime','acpx','node','bao','uv','prime_model_catalog')and isinstance(v,str)and re.fullmatch('[A-Za-z0-9._+-]{1,128}',v)}
   safe['acceptance']={k:v for k,v in acceptance.items()if k in ('sealed','independent_boots','unique_identities')}
   if any(type(acceptance.get(k))is not bool for k in ('sealed','unique_identities'))or type(acceptance.get('independent_boots'))is not int:raise ValueError('CATALOG_SOURCE_ACCEPTANCE_METADATA')
   safe['disk']={k:v for k,v in entry['disk'].items()if k in ('sha256','size_bytes','virtual_size_bytes','minimum_disk_gib','format')}
   safe['runtime_yaml']={k:v for k,v in entry['runtime_yaml'].items()if k in ('sha256','size_bytes')}
   signature=json.dumps(safe,sort_keys=True,separators=(',',':'))
   if identity in identities and identities[identity]!=signature:raise ValueError('CATALOG_SOURCE_PLATFORM_CONFLICT')
   identities[identity]=signature
   out.append(dict(safe,catalog_source=source['id'],catalog_sha256=sha))
   if len(out)>4096:raise ValueError('CATALOG_SOURCES_TOO_MANY_ENTRIES')
 return out

def groups(entries):
 out={}
 for entry in entries:
  key=(entry['catalog_source'],entry['catalog_sha256'])
  out.setdefault(key,[]).append(entry)
 return out

def summaries(entries):
 return [{'id':source,'catalog_sha256':sha,'platforms':[{'architecture':arch,'vm_type':vm}for arch,vm in sorted({(e['architecture'],e['vm_type'])for e in rows})]}for (source,sha),rows in groups(entries).items()]

def capacity(result,entries):
 sources=summaries(entries);by_pin={s['catalog_sha256']:s for s in sources}
 for node in result['nodes']:
  source=by_pin.get(node.get('catalog_sha256'));node['catalog_recognized']=source is not None
  node['catalog_source']=source['id']if source else None
  if not source:
   node['placement_eligible']=False
   node['placement_reasons']=[*node.get('placement_reasons',[]),'catalog_source_unrecognized']
   node['available_for_one_new_vm']={'vcpus':None,'guest_memory_mib':None,'disk_gib':None,'additional_vm_slots':None,'telemetry':'unrecognized_catalog','advisory':True}
 result['catalog_sources']=sources
 return result

def plan(entries,nodes,template,version,arch,cpus,memory,disk):
 choices=[];selected=None
 for (source,sha),rows in groups(entries).items():
  choice={'catalog_source':source,'catalog_sha256':sha,'fits_snapshot':False}
  try:pins=placement.variants(rows,template,version,arch,disk)
  except ValueError as error:
   if str(error)!='NO_ACCEPTED_COMPATIBLE_VARIANT':raise
   choice['reason']='no_accepted_compatible_variant';choices.append(choice);continue
  choice['platforms']=[{'architecture':p['architecture'],'vm_type':p['vm_type']}for p in pins]
  try:envelope=placement.envelope(pins,nodes,cpus,memory,disk,sha)
  except RuntimeError as error:
   if str(error) not in ('ELIGIBLE_PLATFORM_TELEMETRY_UNAVAILABLE','CONSERVATIVE_RESERVATION_ENVELOPE_NO_CURRENT_FIT'):raise
   choice['reason']=str(error);choices.append(choice);continue
  # Avoid knowingly offering a node whose effective quota snapshot says no.
  # The driver remains authoritative when capacity changes after this snapshot.
  envelope=dict(envelope,candidate_node_ids=sorted(n['node_id']for n in envelope['per_node_fit']if n['fits_snapshot_envelope']))
  choice.update(fits_snapshot=True,per_node_fit=envelope['per_node_fit']);choices.append(choice)
  if selected is None:selected={'catalog_source':source,'catalog_sha256':sha,'variants':pins,'reservation_envelope':envelope}
 if selected is None:raise RuntimeError('NO_PINNED_CATALOG_SOURCE_WITH_CURRENT_FIT')
 selected.update(selection_policy='first_configured_source_with_current_fit',catalog_routes=choices,advisory=True)
 return selected
