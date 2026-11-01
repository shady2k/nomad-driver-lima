"""Pure config projection shared with the private review planner."""
import importlib.util
from pathlib import Path
s=importlib.util.spec_from_file_location('lima_review_transition',Path(__file__).resolve().parents[3]/'prepare-lima-only.py')
m=importlib.util.module_from_spec(s)
s.loader.exec_module(m)
def transition(source,config,disabled):
    value,plan=m.candidate(source,config,disabled)
    return {'config':value,'plan':plan}
def repository_profiles(value):
    import importlib.util,re
    if not isinstance(value,dict)or set(value)-{'repository_profile_default','repository_profiles'}or not isinstance(value.get('repository_profiles'),dict):raise ValueError('Exact repository profile configuration required')
    default=value.get('repository_profile_default')
    if default is not None and (not isinstance(default,str)or default not in value['repository_profiles']):raise ValueError('Repository default must name an approved profile')
    module_path=Path(__file__).resolve().parents[4]/'plugin/guest/repository_profile.py'
    spec=importlib.util.spec_from_file_location('reviewed_repository_profile',module_path);guest=importlib.util.module_from_spec(spec);spec.loader.exec_module(guest)
    for identity,profile in value['repository_profiles'].items():
        if not isinstance(identity,str)or not re.fullmatch('[a-z0-9][a-z0-9-]{0,63}',identity)or not isinstance(profile,dict)or profile.get('id')!=identity:raise ValueError('Repository profile identity mismatch')
        arch=profile.get('architectures',['amd64'])
        if not isinstance(arch,list)or not arch:raise ValueError('Repository architectures required')
        for architecture in arch:guest.validate(profile,{'os':'linux','distribution':'ubuntu','version':'24.04','architecture':architecture})
    return {'valid':True,'profiles':sorted(value['repository_profiles']),'default':default}

class FilterModule:
    def filters(self):return {'lima_only_transition':transition,'lima_repository_profiles':repository_profiles}
