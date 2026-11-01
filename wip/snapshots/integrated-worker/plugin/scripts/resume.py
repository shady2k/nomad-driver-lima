"""Retained VM resume plans use new Nomad allocations and fresh sessions only."""
import copy
import re
import uuid


def plan(config, source, session, resource_change=None):
    if str(uuid.UUID(session)) != session:
        raise ValueError('CANONICAL_NEW_SESSION_REQUIRED')
    job = source.get('Job', {})
    if (source.get('Namespace') != config['namespace'] or
            job.get('Meta', {}).get('nomad_workers_owner') != config['owner_id']):
        raise ValueError('OWNED_SOURCE_REQUIRED')
    if source.get('DesiredStatus') != 'stop' or source.get('ClientStatus') not in ('complete','failed'):
        raise ValueError('STOPPED_TERMINAL_SOURCE_REQUIRED_NO_AUTOMATIC_INTERRUPTION')
    tasks = [t for g in job.get('TaskGroups', []) for t in g.get('Tasks', [])
             if t.get('Name') == config['task'] and t.get('Driver') == 'lima']
    if len(tasks) != 1:
        raise ValueError('EXACT_LIMA_SOURCE_REQUIRED')
    events = source.get('TaskStates', {}).get(config['task'], {}).get('Events', [])
    receipts = [e.get('Details', {}) for e in events
                if e.get('DriverMessage') == 'lima vm_stopped'
                and e.get('Details', {}).get('stop_revocation_confirmed') == 'true']
    if not receipts:
        raise ValueError('CONFIRMED_STOP_REVOCATION_RECEIPT_REQUIRED')
    receipt = receipts[-1]
    key = receipt.get('resume_source_key', '')
    node = receipt.get('resume_node_id')
    generation = int(receipt.get('resume_generation', '0'))
    if not re.fullmatch('[a-f0-9]{64}', key) or node != source.get('NodeID') or not node or generation < 1:
        raise ValueError('SOURCE_BINDING_INVALID')
    old = tasks[0]['Config']
    if old.get('worker_session_id') == session:
        raise ValueError('NEW_SESSION_REQUIRED')
    vault = config.get('vault', {})
    if not vault.get('role'):
        raise ValueError('TASK_VAULT_ROLE_REQUIRED')
    command = config.get('guest_bootstrap_command')
    if not isinstance(command, list) or not command or not all(isinstance(v, str) for v in command):
        raise ValueError('RUNTIME_BOOTSTRAP_COMMAND_REQUIRED')
    cfg = {k: copy.deepcopy(v) for k, v in old.items() if k in (
        'template_id', 'template_version', 'template_disk_sha256', 'guest_cpus',
        'memory_mib', 'disk_gib', 'architecture', 'guest_os',
        'template_catalog_sha256', 'template_variants_json', 'repository_profile', 'repository_profile_sha256', 'purpose', 'project', 'task_link')}
    cfg.update(command=[v.replace('{session_id}', session) for v in command],
               worker_session_id=session, creator_agent_id=config['agent_instance_id'],
               creator_session_id=config.get('creator_session_id', session), vault_required=True,
               resume_from_key=key, resume_generation=generation, resume_node_id=node)
    if resource_change is not None:
        desired=resource_change['desired']
        cfg.update(guest_cpus=desired['vcpus'],memory_mib=desired['memory_mib'],disk_gib=desired['disk_gib'],resource_change_reason=resource_change['reason'])
    meta = {k: str(v) for k, v in cfg.items() if k in (
        'creator_agent_id', 'creator_session_id', 'worker_session_id', 'template_id',
        'template_version', 'template_disk_sha256', 'purpose', 'project', 'task_link')}
    meta.update(nomad_workers_owner=config['owner_id'], resumed_from=source['ID'])
    # Preserve native reservations; driver rechecks current topology and policy.
    task = {'Name': config['task'], 'Driver': 'lima', 'Config': cfg,
            'Resources': copy.deepcopy(resource_change['nomad_reservation'] if resource_change is not None else tasks[0]['Resources']),
            'Vault': {'Role': vault['role'], 'Env': False, 'DisableFile': False, 'ChangeMode': 'noop'},
            'LogConfig': {'MaxFiles': 10, 'MaxFileSizeMB': 10}}
    constraints=[{'LTarget': '${node.unique.id}', 'Operand': '=', 'RTarget': node}]
    if resource_change is not None:constraints.append({'LTarget':'${attr.driver.lima.resource_change.stopped}','Operand':'=','RTarget':'true'})
    return {'ID': 'worker-' + session, 'Name': 'resume-' + session[:8],
            'Namespace': config['namespace'], 'Type': 'batch', 'Datacenters': config['datacenters'],
            'Meta': meta, 'Constraints': constraints,
            'TaskGroups': [{'Name': 'worker', 'Count': 1,
                            'RestartPolicy': {'Attempts': 0, 'Mode': 'fail'},
                            'ReschedulePolicy': {'Attempts': 0, 'Unlimited': False}, 'Tasks': [task]}]}
