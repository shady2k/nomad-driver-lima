"""Effective one-new-VM fit; free snapshot is advisory, never a reservation."""
import time

def available(cpu,mem,per,overhead,limits,telemetry,now=None):
 now=time.time() if now is None else now
 def number(v):return type(v)==int and v>=0
 result={'vcpus':max(0,cpu)//per if number(cpu) and type(per)==int and per>0 else None,'guest_memory_mib':max(0,mem-overhead) if number(mem) and number(overhead) else None,'disk_gib':None,'additional_vm_slots':None,'telemetry':'unknown','advisory':True}
 for key,capkey in [('vcpus','max_guest_vcpus'),('guest_memory_mib','max_guest_memory_mib')]:
  cap=limits.get(capkey)
  if type(cap)==int and cap>0 and result[key] is not None:result[key]=min(result[key],cap)
 result['configured_caps']={k:('no_additional_cap' if v==0 else v) for k,v in limits.items() if k!='memory_overhead_mib'}
 if not telemetry.get('known') or not number(telemetry.get('observed_unix')) or not 0<=now-telemetry['observed_unix']<=90:return result
 required=['used_vms','used_vcpus','used_reservation_mib','used_disk_gib','physical_available_mib','disk_floor_mib']
 if not all(number(telemetry.get(k)) for k in required):return result
 result['telemetry']='fresh_driver_admission_snapshot'
 def bound(key,cap,used=0,over=0):
  if type(cap)==int and cap>0:
   n=max(0,cap-used-over);result[key]=n if result[key] is None else min(result[key],n)
 bound('vcpus',limits.get('max_total_vcpus'),telemetry['used_vcpus']);bound('vcpus',limits.get('max_guest_vcpus'))
 bound('guest_memory_mib',telemetry.get('host_memory_limit_mib'),telemetry['used_reservation_mib'],overhead)
 bound('guest_memory_mib',limits.get('max_total_memory_mib'),telemetry['used_reservation_mib'],overhead);bound('guest_memory_mib',limits.get('max_guest_memory_mib'))
 result['disk_gib']=max(0,telemetry['physical_available_mib']-telemetry['disk_floor_mib'])//1024
 bound('disk_gib',limits.get('max_total_disk_gib'),telemetry['used_disk_gib']);bound('disk_gib',limits.get('max_guest_disk_gib'))
 cap=limits.get('max_vms')
 if type(cap)==int and cap>0:result['additional_vm_slots']=max(0,cap-telemetry['used_vms'])
 if result['additional_vm_slots']==0:result.update(vcpus=0,guest_memory_mib=0,disk_gib=0)
 return result
