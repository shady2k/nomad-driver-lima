"""Retained VM resume plans use new Nomad allocations and fresh sessions only."""
import copy
import re
import uuid
import hashlib
import json
import os
import stat


def read_maintenance(path, expected_sha256):
    if not re.fullmatch('[a-f0-9]{64}', expected_sha256 or ''):
        raise ValueError('PINNED_MAINTENANCE_RECEIPT_REQUIRED')
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(fd, 'rb') as f:
        st = os.fstat(f.fileno())
        if not stat.S_ISREG(st.st_mode) or st.st_uid != os.getuid() or stat.S_IMODE(st.st_mode) != 0o600:
            raise ValueError('PRIVATE_MAINTENANCE_RECEIPT_REQUIRED')
        raw = f.read(65537)
    if len(raw) > 65536 or hashlib.sha256(raw).hexdigest() != expected_sha256:
        raise ValueError('MAINTENANCE_RECEIPT_CHANGED')
    return json.loads(raw)


def plan(config, source, session):
    return _plan(config, source, session)


def plan_from_maintenance(config, source, session, observation):
    """Operator-only translation of a real maintenance receipt, never a fake event.

    The new normal job still passes the driver's authoritative retained receipt,
    ownership, fresh-token and RAM admission checks. This file grants no rights.
    """
    binding = observation.get('resume_source', {})
    meta = source.get('Job', {}).get('Meta', {})
    if (observation.get('phase') != 'both_stopped_original_resume_receipt_ready'
            or observation.get('original_retained_receipt') is not True
            or observation.get('duplicate_retained') is not True
            or observation.get('shared_token') is not True
            or not re.fullmatch('[a-f0-9]{64}', observation.get('audit_sha256', ''))
            or not re.fullmatch('[a-f0-9]{64}', binding.get('receipt_sha256', ''))
            or binding.get('allocation') != source.get('ID')
            or binding.get('node') != source.get('NodeID')
            or binding.get('namespace') != source.get('Namespace')
            or binding.get('task') != config.get('task')
            or not binding.get('creator_agent_id')
            or binding.get('creator_agent_id') != config.get('agent_instance_id')
            or binding.get('creator_agent_id') != meta.get('creator_agent_id')
            or binding.get('creator_session_id') != meta.get('creator_session_id')):
        raise ValueError('EXACT_OWNED_MAINTENANCE_RECEIPT_REQUIRED')
    task = next((t for g in source.get('Job', {}).get('TaskGroups', []) for t in g.get('Tasks', []) if t.get('Name') == config.get('task')), {})
    if task.get('Config', {}).get('worker_session_id') != binding.get('session'):
        raise ValueError('MAINTENANCE_SOURCE_SESSION_CHANGED')
    receipt = {'resume_source_key': binding.get('key'), 'resume_node_id': binding.get('node'), 'resume_generation': binding.get('generation')}
    job = _plan(config, source, session, receipt)
    job['Meta'].update(recovery_evidence='operator_maintenance_retained_receipt',
                       recovery_audit_sha256=observation['audit_sha256'],
                       recovery_receipt_sha256=binding['receipt_sha256'])
    return job


def _plan(config, source, session, maintenance_receipt=None):
    if str(uuid.UUID(session)) != session:
        raise ValueError('CANONICAL_NEW_SESSION_REQUIRED')
    job = source.get('Job', {})
    if (source.get('Namespace') != config['namespace'] or
            job.get('Meta', {}).get('nomad_workers_owner') != config['owner_id'] or
            source.get('DesiredStatus') != 'stop' or
            source.get('ClientStatus') not in ('complete', 'failed')):
        raise ValueError('OWNED_TERMINAL_SOURCE_REQUIRED')
    tasks = [t for g in job.get('TaskGroups', []) for t in g.get('Tasks', [])
             if t.get('Name') == config['task'] and t.get('Driver') == 'lima']
    if len(tasks) != 1:
        raise ValueError('EXACT_LIMA_SOURCE_REQUIRED')
    if maintenance_receipt is None:
        events = source.get('TaskStates', {}).get(config['task'], {}).get('Events', [])
        receipts = [e.get('Details', {}) for e in events
                    if e.get('DriverMessage') == 'lima vm_stopped'
                    and e.get('Details', {}).get('stop_revocation_confirmed') == 'true']
        if not receipts:
            raise ValueError('CONFIRMED_STOP_REVOCATION_RECEIPT_REQUIRED')
        receipt = receipts[-1]
    else:
        receipt = maintenance_receipt
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
    meta = {k: str(v) for k, v in cfg.items() if k in (
        'creator_agent_id', 'creator_session_id', 'worker_session_id', 'template_id',
        'template_version', 'template_disk_sha256', 'purpose', 'project', 'task_link')}
    meta.update(nomad_workers_owner=config['owner_id'], resumed_from=source['ID'])
    # Preserve native reservations; driver rechecks current topology and policy.
    task = {'Name': config['task'], 'Driver': 'lima', 'Config': cfg,
            'Resources': copy.deepcopy(tasks[0]['Resources']),
            'Vault': {'Role': vault['role'], 'Env': False, 'DisableFile': False, 'ChangeMode': 'noop'},
            'LogConfig': {'MaxFiles': 10, 'MaxFileSizeMB': 10}}
    return {'ID': 'worker-' + session, 'Name': 'resume-' + session[:8],
            'Namespace': config['namespace'], 'Type': 'batch', 'Datacenters': config['datacenters'],
            'Meta': meta, 'Constraints': [{'LTarget': '${node.unique.id}', 'Operand': '=', 'RTarget': node}],
            'TaskGroups': [{'Name': 'worker', 'Count': 1, 'Disconnect': __import__('disconnect_policy').group_strategy(config),
                            'RestartPolicy': {'Attempts': 0, 'Mode': 'fail'},
                            'ReschedulePolicy': {'Attempts': 0, 'Unlimited': False}, 'Tasks': [task]}]}
