"""Configure visible passwordless entry plus conditional passkey UI."""
import json,hashlib
from django.db import transaction
from authentik.flows.models import Flow,FlowStageBinding
from authentik.policies.models import PolicyBinding
from authentik.stages.identification.models import IdentificationStage
from authentik.stages.authenticator_validate.models import AuthenticatorValidateStage
from authentik.stages.authenticator_webauthn.models import WebAuthnDevice

c=PASSKEY_CONFIG
assert set(c)=={'flow_slug','identification_name','validation_name'}
passwordless_slug='passkey-authentication-flow'
desired={'device_classes':['webauthn'],'not_configured_action':'deny','last_auth_threshold':'seconds=0','webauthn_user_verification':'required','webauthn_hints':[]}
def snapshot():
 flow=Flow.objects.get(slug=c['flow_slug']);ident=IdentificationStage.objects.get(name=c['identification_name'])
 bindings=list(FlowStageBinding.objects.filter(target=flow).order_by('pk').values('pk','pbm_uuid','stage_id','order','evaluate_on_plan','re_evaluate_policies','invalid_response_action','policy_engine_mode'))
 assert any(v['stage_id']==ident.pk for v in bindings)
 targets=[flow.pbm_uuid]+[v['pbm_uuid'] for v in bindings]
 policies=list(PolicyBinding.objects.filter(target_id__in=targets).order_by('pk').values('pk','target_id','policy_id','user_id','group_id','enabled','negate','order','failure_result'))
 existing=AuthenticatorValidateStage.objects.filter(name=c['validation_name']).first()
 return {'identification':{'pk':str(ident.pk),'webauthn_stage_id':str(ident.webauthn_stage_id) if ident.webauthn_stage_id else None,'passwordless_flow_id':str(ident.passwordless_flow_id) if ident.passwordless_flow_id else None,'password_stage_id':str(ident.password_stage_id) if ident.password_stage_id else None,'user_fields':ident.user_fields,'enable_remember_me':ident.enable_remember_me},'bindings':bindings,'policies':policies,'validation_before':None if existing is None else {'pk':str(existing.pk),**{k:getattr(existing,k) for k in desired}},'webauthn_device_count':WebAuthnDevice.objects.count(),'passwordless_flow':list(Flow.objects.filter(slug=passwordless_slug).values('pk','slug','designation','authentication','title')),'passwordless_bindings':list(FlowStageBinding.objects.filter(target__slug=passwordless_slug).order_by('order').values('stage_id','order','evaluate_on_plan','re_evaluate_policies'))}
def digest(value):return hashlib.sha256(json.dumps(value,sort_keys=True,default=str,separators=(',',':')).encode()).hexdigest()
if PASSKEY_MODE=='inspect':
 before=snapshot();print('PASSKEY_SNAPSHOT:'+json.dumps({'guard_sha256':digest(before),'snapshot':before},default=str))
elif PASSKEY_MODE=='apply':
 with transaction.atomic():
  ident=IdentificationStage.objects.select_for_update().get(name=c['identification_name'])
  flow=Flow.objects.select_for_update().get(slug=c['flow_slug'])
  list(FlowStageBinding.objects.select_for_update().filter(target=flow))
  before=snapshot();assert digest(before)==EXPECTED_GUARD,'Concurrent change; re-review'
  assert ident.password_stage_id is None and not ident.enable_remember_me,'Unexpected first-screen configuration; review required'
  stage=AuthenticatorValidateStage.objects.select_for_update().filter(name=c['validation_name']).first();changed=False
  if stage is None:stage=AuthenticatorValidateStage.objects.create(name=c['validation_name'],**desired);changed=True
  else:assert all(getattr(stage,k)==v for k,v in desired.items()),'Conflicting stage settings; do not overwrite'
  assert ident.webauthn_stage_id in (None,stage.pk),'Different configured WebAuthn entry; review required'
  if ident.webauthn_stage_id!=stage.pk:ident.webauthn_stage=stage;ident.save(update_fields=['webauthn_stage']);changed=True
  pw=Flow.objects.select_for_update().filter(slug=passwordless_slug).first()
  if pw is None:
   pw=Flow.objects.create(slug=passwordless_slug,name='Passkey sign-in',title='Вход по passkey',designation=flow.designation,authentication=flow.authentication,policy_engine_mode=flow.policy_engine_mode,denied_action=flow.denied_action);changed=True
  else:assert pw.designation==flow.designation and pw.authentication==flow.authentication and pw.title=='Вход по passkey','Unexpected passwordless flow'
  # Preserve existing reputation and additional verification/login semantics.
  original=list(FlowStageBinding.objects.filter(target=flow).order_by('order'))
  selected=[b for b in original if b.stage.__class__.__name__ in ('DenyStage','AuthenticatorValidateStage','UserLoginStage')]
  assert any(b.stage.__class__.__name__=='DenyStage' for b in selected)
  assert any(b.stage.__class__.__name__=='UserLoginStage' for b in selected)
  expected=[(stage.pk,10)]+[(b.stage_id,b.order) for b in selected]
  assert not FlowStageBinding.objects.filter(target=pw).exclude(stage_id__in=[v[0] for v in expected]).exists(),'Unexpected passwordless stage'
  vb,created=FlowStageBinding.objects.get_or_create(target=pw,stage=stage,order=10,defaults={'evaluate_on_plan':False,'re_evaluate_policies':True});changed |= created
  assert vb.evaluate_on_plan is False and vb.re_evaluate_policies is True
  for b in selected:
   fields={k:getattr(b,k) for k in ['evaluate_on_plan','re_evaluate_policies','invalid_response_action','policy_engine_mode']}
   nb,created=FlowStageBinding.objects.get_or_create(target=pw,stage_id=b.stage_id,order=b.order,defaults=fields);changed |= created
   assert all(getattr(nb,k)==v for k,v in fields.items())
   for pb in PolicyBinding.objects.filter(target=b):
    kwargs={f.attname:getattr(pb,f.attname) for f in PolicyBinding._meta.concrete_fields if not f.primary_key and f.name!='target'}
    _,created=PolicyBinding.objects.get_or_create(target=nb,**kwargs);changed |= created
  for pb in PolicyBinding.objects.filter(target=flow):
   kwargs={f.attname:getattr(pb,f.attname) for f in PolicyBinding._meta.concrete_fields if not f.primary_key and f.name!='target'}
   _,created=PolicyBinding.objects.get_or_create(target=pw,**kwargs);changed |= created
  assert ident.passwordless_flow_id in (None,pw.pk),'Conflicting linked flow'
  if ident.passwordless_flow_id!=pw.pk:ident.passwordless_flow=pw;ident.save(update_fields=['passwordless_flow']);changed=True
  after=snapshot()
  assert after['bindings']==before['bindings'] and after['policies']==before['policies'],'Existing login/reputation/password/MFA binding changed'
  assert after['webauthn_device_count']==before['webauthn_device_count']
  for k,v in before['identification'].items():
   if k not in ('webauthn_stage_id','passwordless_flow_id'):assert after['identification'][k]==v
  assert after['identification']['webauthn_stage_id']==str(stage.pk)
  print('PASSKEY_RESULT:'+json.dumps({'changed':changed,'validation_name':stage.name,'device_classes':stage.device_classes,'user_verification':stage.webauthn_user_verification,'not_configured_action':stage.not_configured_action,'threshold':stage.last_auth_threshold,'conditional_ui_enabled':True,'visible_passwordless_flow_link':True,'passwordless_flow_slug':passwordless_slug,'first_screen_username_password_fallback_preserved':True,'existing_stage_bindings_unchanged':True,'reputation_and_skip_policies_unchanged':True,'devices_modified':False,'owner_biometric_test_performed':False}))
else:raise ValueError('Unsupported mode')
