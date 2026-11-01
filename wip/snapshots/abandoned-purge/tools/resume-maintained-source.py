#!/usr/bin/env python3
"""Operator-only ordinary resume using a pinned real maintenance completion.

No token/host policy is accepted. Existing Nomad identity, native creator record,
config and driver checks remain authoritative. No prompt is submitted.
"""
import argparse
import json
import pathlib
import sys

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1] / 'plugin' / 'scripts'))
from catalog_cli import CatalogClient


def main():
    p = argparse.ArgumentParser(description=__doc__)
    for field in ('config', 'session', 'source-allocation', 'creator-record', 'maintenance-receipt', 'maintenance-receipt-sha256'):
        p.add_argument('--' + field, required=True)
    p.add_argument('--execute', action='store_true', help='submit one ordinary new resume allocation; never replay prompt')
    a = p.parse_args()
    if not a.execute:
        p.error('--execute is required; run recover-owned-pair without confirmation for the read-only maintenance preflight first')
    client = CatalogClient(a.config)
    result = client.resume(a.session, a.source_allocation, a.creator_record,
                           maintenance_receipt_path=a.maintenance_receipt,
                           maintenance_receipt_sha256=a.maintenance_receipt_sha256)
    print(json.dumps(result))


if __name__ == '__main__':
    main()
