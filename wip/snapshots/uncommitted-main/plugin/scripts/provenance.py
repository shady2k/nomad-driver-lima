"""Descriptive source/task metadata, never identity or authenticated liveness."""
import re
from secure_logs import Redactor

def text(value,field,minimum=1,maximum=256):
 if not isinstance(value,str)or not minimum<=len(value.strip())<=maximum or any(ord(c)<32 for c in value)or Redactor().text(value)!=value:raise ValueError('INVALID_DESCRIPTIVE_'+field.upper())
 return value.strip()
def descriptions(config,native,task_title,name=None,kind=None,link='',purpose=''):
 title=text(task_title,'task_title',minimum=5)
 if title.casefold()in ('unknown','busy-terminal','terminal','worker','agent','environment','tests')or title.casefold()==purpose.casefold():raise ValueError('MEANINGFUL_TASK_TITLE_REQUIRED')
 defaults=config.get('source_agent',{})
 def select(field,explicit):
  actual=native.get('native_agent_type')
  if actual:return text(actual,field),'native_hook.agent_type'
  if explicit:return text(explicit,field),'descriptive_input'
  if defaults.get(field):return text(defaults[field],field),'operator_config'
  raise ValueError('SOURCE_AGENT_NAME_TYPE_REQUIRED_DESCRIPTIVE_INPUT')
 source_name,name_evidence=select('name',name);source_type,type_evidence=select('type',kind)
 if source_name.casefold()in ('unknown','agent','worker')or source_type.casefold()=='unknown':raise ValueError('DESCRIPTIVE_SOURCE_REQUIRED')
 return {'source_agent_name':source_name,'source_agent_type':source_type,'source_agent_name_evidence':name_evidence,'source_agent_type_evidence':type_evidence,'source_native_agent_id':native.get('native_agent_id',''),'source_start_source':native.get('start_source','unknown'),'source_task_title':title,'source_task_link_status':'provided'if link else'not_provided'}
