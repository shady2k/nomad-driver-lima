"""Operator-only profile selection. Agent requests choose IDs, never endpoints."""
import importlib.util,pathlib
spec=importlib.util.spec_from_file_location('guest_repository_profile',pathlib.Path(__file__).parents[1]/'guest/repository_profile.py');guest=importlib.util.module_from_spec(spec);spec.loader.exec_module(guest)

def select(operator_config,requested,platform):
 selected=operator_config.get('repository_profile_default')if requested is None else requested
 if selected is None:return {'enabled':False,'profile':None,'profile_sha256':None}
 if not isinstance(selected,str):raise guest.ProfileError('REPOSITORY_PROFILE_ID_REQUIRED')
 profiles=operator_config.get('repository_profiles',{})
 if not isinstance(profiles,dict)or selected not in profiles:raise guest.ProfileError('REPOSITORY_PROFILE_NOT_APPROVED')
 profile=guest.validate(profiles[selected],platform)
 if profile['id']!=selected:raise guest.ProfileError('REPOSITORY_PROFILE_ID_MISMATCH')
 return {'enabled':True,'profile':profile,'profile_sha256':guest.digest(profile)}

def apply_payload(selected,expected_guest_identity):
 if not selected.get('enabled'):return {'profile':None}
 profile=selected['profile']
 if guest.digest(profile)!=selected.get('profile_sha256'):raise guest.ProfileError('REPOSITORY_PROFILE_DIGEST_MISMATCH')
 return {'profile':profile,'profile_sha256':selected['profile_sha256'],'expected_guest_identity':expected_guest_identity}

def execution_reference(selected,apply_receipt):
 if not selected.get('enabled'):return None
 if apply_receipt.get('profile_sha256')!=selected['profile_sha256']or not apply_receipt.get('enabled')or not __import__('re').fullmatch('[a-f0-9]{64}',apply_receipt.get('environment_sha256','')):raise guest.ProfileError('REPOSITORY_APPLY_RECEIPT_MISMATCH')
 return {'profile_sha256':selected['profile_sha256'],'environment_sha256':apply_receipt['environment_sha256']}
