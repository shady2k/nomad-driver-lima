"""Supported Brand default flow with an always-visible password fallback."""
import json,hashlib
from django.db import transaction
from authentik.brands.models import Brand
from authentik.flows.models import Flow
from authentik.tenants.models import Tenant
c=PASSKEY_FIRST_CONFIG
assert {'brand_id','fallback_url','activate_default'}<=set(c) and not set(c)-{'brand_id','fallback_url','activate_default','previous_fallback_url','restore_standard'}
assert c['fallback_url']=='/if/flow/default-authentication-flow/' or (c['fallback_url'].startswith('https://') and c['fallback_url'].endswith('/if/flow/default-authentication-flow/'))
link={'name':'Войти с паролем и TOTP','href':c['fallback_url']}
def snapshot():
 b=Brand.objects.get(pk=c['brand_id']);t=Tenant.objects.get(schema_name='public')
 return {'brand_id':str(b.pk),'authentication_flow':str(b.flow_authentication_id),'footer_links':t.footer_links}
def sha(x):return hashlib.sha256(json.dumps(x,sort_keys=True).encode()).hexdigest()
if PASSKEY_FIRST_MODE=='inspect':
 s=snapshot();print('FIRST_SNAPSHOT:'+json.dumps({'snapshot':s,'guard':sha(s)}))
elif PASSKEY_FIRST_MODE=='apply':
 with transaction.atomic():
  b=Brand.objects.select_for_update().get(pk=c['brand_id']);t=Tenant.objects.select_for_update().get(schema_name='public')
  before=snapshot();assert sha(before)==EXPECTED_GUARD,'Concurrent change'
  old=Flow.objects.get(slug='default-authentication-flow');new=Flow.objects.get(slug='passkey-authentication-flow')
  assert b.flow_authentication_id in (old.pk,new.pk)
  changed=False
  previous={'name':link['name'],'href':c.get('previous_fallback_url')}
  if previous in t.footer_links and previous!=link:
   t.footer_links=[v for v in t.footer_links if v!=previous];t.save(update_fields=['footer_links']);changed=True
  restore=c.get('restore_standard',False)
  if restore and link in t.footer_links:
   t.footer_links=[v for v in t.footer_links if v!=link];t.save(update_fields=['footer_links']);changed=True
  if not restore and link not in t.footer_links:
   t.footer_links=[*t.footer_links,link];t.save(update_fields=['footer_links']);changed=True
  if c['activate_default'] and b.flow_authentication_id!=new.pk:
   b.flow_authentication=new;b.save(update_fields=['flow_authentication']);changed=True
  if restore and b.flow_authentication_id!=old.pk:
   b.flow_authentication=old;b.save(update_fields=['flow_authentication']);changed=True
  assert all(v in t.footer_links for v in before['footer_links'] if v!=previous and (not restore or v!=link))
  print('FIRST_RESULT:'+json.dumps({'changed':changed,'default_passkey':b.flow_authentication_id==new.pk,'standard_login':b.flow_authentication_id==old.pk,'custom_fallback_removed':link not in t.footer_links,'visible_password_fallback_configured':link in t.footer_links,'other_brand_settings_modified':False,'policies_or_devices_modified':False}))
else:raise ValueError('Unsupported mode')
