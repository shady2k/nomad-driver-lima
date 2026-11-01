"""Native Nomad finite disconnect grace for new owned worker jobs only."""
DEFAULT_SECONDS=30*60

def group_strategy(config):
 policy=config.get('disconnect_policy',{'lost_after_seconds':DEFAULT_SECONDS,'replace':False,'reconcile':'keep_original'})
 if not isinstance(policy,dict)or set(policy)!={'lost_after_seconds','replace','reconcile'}:raise ValueError('EXACT_DISCONNECT_POLICY_REQUIRED')
 seconds=policy['lost_after_seconds']
 if type(seconds)is not int or not 0<seconds<=((1<<63)-1)//1000000000:raise ValueError('FINITE_POSITIVE_DISCONNECT_GRACE_REQUIRED')
 if policy['replace']is not False or policy['reconcile']!='keep_original':raise ValueError('DISCONNECT_MUST_KEEP_ORIGINAL_WITHOUT_REPLACEMENT')
 # API Duration is nanoseconds. Absent StopOnClientAfter is intentional;
 # even an explicit zero has different client-side semantics in Nomad.
 return {'LostAfter':seconds*1000000000,'Replace':False,'Reconcile':'keep_original'}
