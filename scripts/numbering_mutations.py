"""Isolated SQL function faults for transactional warehouse document numbers.

The shared runner clones the disposable DB, fetches pg_get_functiondef and
replaces one site. This module never rewrites a published migration or DB.
"""
import copy
import re


_ALLOCATE = 'warehouse.allocate_document_number(uuid,uuid,text,timestamptz)'
_RESERVE = 'warehouse.reserve_document_number(uuid,uuid,text,text)'
_COMMAND = 'warehouse.command_document(uuid,uuid,uuid,jsonb)'


def _case(name, signature, before, after, test):
    return dict(name='numbering_'+name, kind='sql_function', domain='document_numbering', signature=signature, before=before, after=after, pattern='^TestNumberingCritic'+test+'$', package='./internal/postgres')


_CASES = [
    _case('counter_increments_twice', _ALLOCATE, 'last_value=last_value+1', 'last_value=last_value+2', 'AutoKindsAndDurableReplay'),
    _case('counter_does_not_increment', _ALLOCATE, 'last_value=last_value+1', 'last_value=last_value', 'AutoKindsAndDurableReplay'),
    _case('warehouse_timezone_ignored', _ALLOCATE, 'p_at AT TIME ZONE zone', "p_at AT TIME ZONE 'UTC'", 'WarehouseYearBoundaryAndMinimumWidth'),
    _case('receipt_and_transfer_prefix_swapped', _ALLOCATE, "WHEN 'receipt' THEN 'PZ' ELSE 'MM'", "WHEN 'receipt' THEN 'MM' ELSE 'PZ'", 'AutoKindsAndDurableReplay'),
    _case('large_number_truncated', _ALLOCATE, 'greatest(6,length(serial_value::text))', '6', 'WarehouseYearBoundaryAndMinimumWidth'),
    _case('manual_reservation_removed', _COMMAND, "PERFORM warehouse.reserve_document_number(org,wh,p_input->>'kind',v_number);", 'NULL;', 'ManualLegacyAndWarehouseScope'),
    _case('manual_rewinds_counter', _RESERVE, 'greatest(counters.last_value,excluded.last_value)', 'excluded.last_value', 'ManualLegacyAndWarehouseScope'),
    _case('manual_transfer_uses_receipt_prefix', _RESERVE, "prefix:=CASE p_kind WHEN 'receipt' THEN 'PZ' ELSE 'MM' END;", "prefix:='PZ';", 'ManualLegacyAndWarehouseScope'),
    _case('administrative_collision_probe_removed', _ALLOCATE, 'IF NOT EXISTS(SELECT 1 FROM warehouse.documents', 'IF true OR NOT EXISTS(SELECT 1 FROM warehouse.documents', 'AdministrativeLegacyCollision'),
    _case('replay_allocates_another_number', _COMMAND, 'RETURN previous.result;', "IF p_document IS NULL AND coalesce(p_input->>'number','')='' THEN PERFORM warehouse.allocate_document_number(org,wh,p_input->>'kind',created); END IF; RETURN previous.result;", 'AutoKindsAndDurableReplay'),
]
_NAMES = frozenset(case['name'] for case in _CASES)
_PATTERNS = frozenset(case['pattern'] for case in _CASES)


def load_numbering_mutations(cases=None):
    cases = copy.deepcopy(_CASES if cases is None else cases)
    if not isinstance(cases, list) or len(cases) != 10 or {case.get('name') for case in cases if isinstance(case, dict)} != _NAMES:
        raise ValueError('Missing or duplicate document numbering mutation coverage')
    identities = set()
    for case in cases:
        if not isinstance(case, dict) or case.get('kind') != 'sql_function' or case.get('domain') != 'document_numbering' or case.get('package') != './internal/postgres' or case.get('signature') not in (_ALLOCATE, _RESERVE, _COMMAND):
            raise ValueError('Invalid document numbering mutation target')
        if not isinstance(case.get('before'), str) or not case['before'] or not isinstance(case.get('after'), str) or not case['after'] or case['before'] == case['after']:
            raise ValueError('Numbering mutation must change a known source site')
        if not isinstance(case.get('pattern'), str) or case['pattern'] not in _PATTERNS or not re.fullmatch(r'\^TestNumberingCritic[A-Za-z0-9]+\$', case['pattern']):
            raise ValueError('Numbering mutation requires an explicit critic behavior test')
        identity = (case['signature'], case['before'], case['after'])
        if identity in identities:
            raise ValueError('Duplicate document numbering mutation fault')
        identities.add(identity)
    return cases


def mutate_numbering_function(source, case):
    before, after = case['before'], case['after']
    if before == after or source.count(before) != 1:
        raise ValueError('Numbering mutation must change exactly one source site')
    return source.replace(before, after, 1)
