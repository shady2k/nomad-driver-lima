"""Descriptive task provenance. Never identity, model selection or proof of life."""
from urllib.parse import urlsplit
from secure_logs import Redactor

def text(value,field,minimum=1,maximum=256):
 if not isinstance(value,str)or not minimum<=len(value.strip())<=maximum or any(ord(c)<32 for c in value)or Redactor().text(value)!=value:raise ValueError('INVALID_DESCRIPTIVE_'+field.upper())
 return value.strip()
def descriptions(config,native,task_title,name=None,kind=None,link='',purpose=''):
 if task_title is None or not str(task_title).strip():raise ValueError('TASK_TITLE_REQUIRED: provide --task-title describing the concrete task; no job was submitted')
 title=text(task_title,'task_title',minimum=5)
 if title.casefold()in ('unknown','busy-terminal','terminal','worker','agent','environment','tests'):raise ValueError('MEANINGFUL_TASK_TITLE_REQUIRED')
 defaults=config.get('source_agent',{})
 if not isinstance(defaults,dict)or set(defaults)-{'name','type'}:raise ValueError('SOURCE_AGENT_CONFIGURATION_INVALID')
 def select(field,explicit):
  actual=native.get('native_agent_'+field)
  if actual:return text(actual,field),'native_hook.agent_'+field
  if explicit:return text(explicit,field),'descriptive_input'
  if defaults.get(field):return text(defaults[field],field),'operator_config'
  raise ValueError('SOURCE_AGENT_NAME_TYPE_REQUIRED_DESCRIPTIVE_INPUT')
 source_name,name_evidence=select('name',name);source_type,type_evidence=select('type',kind)
 if source_name.casefold()in ('unknown','agent','worker')or source_type.casefold()=='unknown':raise ValueError('DESCRIPTIVE_SOURCE_REQUIRED')
 if link:
  link=text(link,'task_link',maximum=2048);u=urlsplit(link)
  if u.scheme not in ('https','http')or not u.hostname or u.username or u.password or u.query:raise ValueError('CREDENTIAL_FREE_TASK_LINK_REQUIRED')
 return {'source_agent_name':source_name,'source_agent_type':source_type,'source_agent_name_evidence':name_evidence,'source_agent_type_evidence':type_evidence,'source_native_agent_id':text(native['native_agent_id'],'native_agent_id')if native.get('native_agent_id')else'','source_start_source':text(native.get('start_source','unknown'),'start_source'),'source_task_title':title,'source_task_link_status':'provided'if link else'not_provided'}

def resumed_descriptions(config,native,original):
 """Keep original task context, but never attribute old names to a new creator."""
 defaults=config.get('source_agent',{})
 if not isinstance(defaults,dict)or set(defaults)-{'name','type'}:raise ValueError('SOURCE_AGENT_CONFIGURATION_INVALID')
 result={}
 for field in ('name','type'):
  value=native.get('native_agent_'+field)or defaults.get(field)
  result['source_agent_'+field]=text(value,field)if value else''
  result['source_agent_'+field+'_evidence']=('native_hook.agent_'+field if native.get('native_agent_'+field)else'operator_config')if value else'not_observed_or_configured'
  original_value=original.get('source_agent_'+field)
  result['original_source_agent_'+field]=text(original_value,field)if original_value else''
 title=original.get('source_task_title')
 result.update(source_task_title=text(title,'task_title',minimum=5)if title else'',source_task_title_status='inherited_from_source_task'if title else'not_provided',source_native_agent_id=text(native['native_agent_id'],'native_agent_id')if native.get('native_agent_id')else'',source_native_session_id=native['native_session_id'],source_native_session_status='observed',source_start_source=text(native.get('start_source','unknown'),'start_source'),source_identity_evidence='descriptive_metadata_not_identity_or_liveness')
 return result
