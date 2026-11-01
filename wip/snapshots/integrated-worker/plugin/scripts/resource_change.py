"""Stopped-only native resource-change planning; Nomad and driver are authoritative."""

def plan(config, source, request, node):
    task = config['task']
    tasks = [t for g in source.get('Job', {}).get('TaskGroups', []) for t in g.get('Tasks', [])
             if t.get('Name') == task and t.get('Driver') == 'lima']
    if len(tasks) != 1:
        raise ValueError('EXACT_LIMA_SOURCE_REQUIRED')
    old = tasks[0]['Config']
    current = {'vcpus': old['guest_cpus'], 'memory_mib': old['memory_mib'], 'disk_gib': old['disk_gib']}
    desired = dict(current)
    changes = request.get('desired', {})
    if not changes or set(changes) - set(current):
        raise ValueError('EXPLICIT_DESIRED_RESOURCES_REQUIRED')
    desired.update(changes)
    if any(type(n) != int or n < 1 for n in desired.values()) or desired['memory_mib'] < 256 or desired['disk_gib'] > 1048576:
        raise ValueError('INVALID_DESIRED_RESOURCES')
    reason = request.get('reason', '')
    if not isinstance(reason, str) or not reason.strip() or len(reason.encode()) > 1024 or any(c in reason for c in '\x00\r\n'):
        raise ValueError('BOUNDED_RESOURCE_CHANGE_REASON_REQUIRED')
    if desired['disk_gib'] < current['disk_gib']:
        raise ValueError('DISK_SHRINK_FORBIDDEN')
    if desired == current:
        raise ValueError('RESOURCES_ALREADY_AT_TARGET_NO_NEW_ALLOCATION')
    if node.get('stopped_resource_change') is not True:
        raise ValueError('STOPPED_RESOURCE_CHANGE_DRIVER_UNAVAILABLE')
    if old.get('guest_os') != 'linux' or node.get('node_id') != source.get('NodeID') or not node.get('placement_eligible'):
        raise ValueError('ORIGINAL_ELIGIBLE_LINUX_NODE_REQUIRED')
    per = node.get('cpu_mhz_per_vcpu')
    overhead = node.get('memory_overhead_mib_per_vm')
    if type(per) != int or per < 1 or type(overhead) != int or overhead < 1:
        raise ValueError('NATIVE_RESERVATION_CONVERSION_UNKNOWN')
    cpu = desired['vcpus'] * per
    memory = desired['memory_mib'] + overhead
    if node.get('free_cpu_mhz', -1) < cpu or node.get('free_reservation_mib', -1) < memory:
        raise ValueError('ORIGINAL_NODE_RESERVATION_CAPACITY_INSUFFICIENT')
    limits = node.get('node_operator_limits', {})
    for field, limit in [('vcpus', 'max_guest_vcpus'), ('memory_mib', 'max_guest_memory_mib'), ('disk_gib', 'max_guest_disk_gib')]:
        maximum = limits.get(limit)
        if type(maximum) == int and maximum > 0 and desired[field] > maximum:
            raise ValueError('RESOURCE_CHANGE_EXCEEDS_OPERATOR_LIMIT')
    limits_request=config.get('request_limits',{})
    if not isinstance(limits_request,dict):
        raise ValueError('CALLER_REQUEST_LIMIT_CONFIGURATION_INVALID')
    for field in desired:
        maximum=limits_request.get(field,0)
        if type(maximum)!=int or maximum<0:
            raise ValueError('CALLER_REQUEST_LIMIT_CONFIGURATION_INVALID')
        if maximum>0 and desired[field]>maximum:
            raise ValueError('RESOURCE_CHANGE_EXCEEDS_CALLER_REQUEST_LIMIT')
    delta = desired['disk_gib'] - current['disk_gib']
    telemetry = node.get('occupied', {}).get('driver', {})
    if delta:
        free, floor, used = (telemetry.get(k) for k in ('physical_available_mib', 'disk_floor_mib', 'used_disk_gib'))
        if telemetry.get('known') is not True or any(type(n) != int or n < 0 for n in (free, floor, used)):
            raise ValueError('DISK_CAPACITY_UNKNOWN')
        if free < delta * 1024 + floor:
            raise ValueError('PHYSICAL_DISK_GROWTH_HEADROOM_INSUFFICIENT')
        maximum = limits.get('max_total_disk_gib')
        if type(maximum) == int and maximum > 0 and used + delta > maximum:
            raise ValueError('NODE_DISK_QUOTA_EXCEEDED')
    return {'current': current, 'desired': desired, 'reason': reason,
            'requires_stopped_source': True, 'automatic_stop': False, 'online_hotplug': False,
            'node_id': source['NodeID'], 'disk_delta_gib': delta,
            'nomad_reservation': {'CPU': cpu, 'MemoryMB': memory},
            'rootfs_growth': 'verify_after_native_boot_before_workload' if delta else 'not_requested',
            'capacity_evidence': 'advisory; driver rechecks quota and physical free space'}
