"""Additive owned-lab catalogs and audited, replayable opening PZ documents.

This is an explicit operator fixture, not a general inventory import API.
Credentials, employee identities and existing accounting rows are preserved.
"""
import argparse
import copy
import json
import os
from pathlib import Path
import subprocess
import uuid

import workplace

SCOPE_KEYS = ('instance_id', 'organization_id', 'warehouse_id', 'owner_id',
              'position_receiving_id', 'location_receiving_id')

# SKU, employee-facing Polish name, initial place, initial piece quantity.
METAL = (
    ('SR-M6X20', 'Śruba M6 × 20, ocynkowana', 'A1', 1200),
    ('SR-M8X40', 'Śruba M8 × 40, ocynkowana', 'A1', 600),
    ('SR-M10X50', 'Śruba M10 × 50, ocynkowana', 'A2', 320),
    ('NAK-M6', 'Nakrętka M6, ocynkowana', 'A2', 1500),
    ('NAK-M8', 'Nakrętka M8, ocynkowana', 'A2', 140),
    ('NAK-M10', 'Nakrętka M10, ocynkowana', 'A3', 400),
    ('POD-M6', 'Podkładka M6, ocynkowana', 'A3', 1800),
    ('POD-M8', 'Podkładka M8, ocynkowana', 'A3', 1100),
    ('POD-M10', 'Podkładka M10, ocynkowana', 'A3', 600),
    ('NIT-4X10', 'Nit zrywalny aluminiowy 4 × 10 mm', 'A4', 2400),
    ('NIT-4X16', 'Nit zrywalny aluminiowy 4 × 16 mm', 'A4', 800),
    ('AL-500X300X2', 'Formatka aluminiowa 500 × 300 × 2 mm', 'B1', 70),
    ('AL-1000X500X2', 'Formatka aluminiowa 1000 × 500 × 2 mm', 'B1', 36),
    ('AL-1000X1000X3', 'Formatka aluminiowa 1000 × 1000 × 3 mm', 'B2', 12),
    ('ST-1800X800X15', 'Formatka stalowa 1800 × 800 × 1,5 mm', 'B2', 18),
    ('ST-1800X400X15', 'Formatka stalowa 1800 × 400 × 1,5 mm', 'B3', 30),
    ('ST-800X400X15', 'Formatka stalowa 800 × 400 × 1,5 mm', 'B3', 50),
    ('ZAW-60', 'Zawias stalowy 60 mm', 'A4', 120),
    ('ZAM-SZAFA', 'Zamek do szafy metalowej, komplet z kluczami', 'A4', 8),
    ('NOZ-REG', 'Stopka regulowana do szafy M10', 'A3', 80),
    ('PROFIL-20X20', 'Profil stalowy 20 × 20 × 2 mm, odcinek 2 m', 'B3', 54),
    ('LAK-RAL7035', 'Lakier RAL 7035, puszka 1 l', 'A4', 9),
    ('SZAFA-1800', 'Szafa metalowa 1800 × 800 × 400 mm, RAL 7035', 'B4', 3),
    ('SZAFKA-900', 'Szafka metalowa 900 × 600 × 400 mm, RAL 7035', 'B4', 7),
)

STATIONERY = (
    ('NOT-27-CZ-A5', 'Notes 2027 czerwony A5', 'A1', 240),
    ('NOT-27-BI-A5', 'Notes 2027 biały A5', 'A1', 180),
    ('NOT-27-CA-A5', 'Notes 2027 czarny A5', 'A2', 120),
    ('NOT-27-CZ-A6', 'Notes 2027 czerwony A6', 'A2', 320),
    ('NOT-27-BI-A6', 'Notes 2027 biały A6', 'A2', 220),
    ('NOT-27-CA-A6', 'Notes 2027 czarny A6', 'A3', 160),
    ('NOT-27-CZ-B5', 'Notes 2027 czerwony B5', 'B1', 80),
    ('NOT-27-BI-B5', 'Notes 2027 biały B5', 'B1', 60),
    ('NOT-27-CA-B5', 'Notes 2027 czarny B5', 'B1', 70),
    ('NOT-26-CZ-A5', 'Notes 2026 czerwony A5 · końcówka serii', 'A4', 37),
    ('NOT-26-BI-A5', 'Notes 2026 biały A5 · końcówka serii', 'A4', 12),
    ('NOT-26-CA-A6', 'Notes 2026 czarny A6 · końcówka serii', 'A4', 24),
    ('WKL-A5-KR', 'Wkład do notesu A5, kratka, 100 kartek', 'B2', 350),
    ('WKL-A5-LI', 'Wkład do notesu A5, linia, 100 kartek', 'B2', 280),
    ('WKL-A6-KR', 'Wkład do notesu A6, kratka, 100 kartek', 'B2', 500),
    ('WKL-B5-KR', 'Wkład do notesu B5, kratka, 100 kartek', 'B3', 90),
    ('KAL-27-CZ-A5', 'Kalendarz 2027 książkowy A5 czerwony', 'B3', 200),
    ('KAL-27-BI-A5', 'Kalendarz 2027 książkowy A5 biały', 'B3', 120),
    ('KAL-27-BI-A4', 'Kalendarz 2027 biurkowy A4 biały', 'B4', 180),
    ('KAL-27-SC-A3', 'Kalendarz 2027 ścienny A3', 'B4', 150),
    ('KAL-27-SC-A2', 'Kalendarz 2027 ścienny A2', 'B4', 70),
    ('GUM-A5', 'Gumka zamykająca do notesu A5', 'A3', 700),
    ('ETUI-A5', 'Etui kartonowe na notes A5', 'B4', 250),
    ('ZEST-26-CZ-A5', 'Zestaw: notes 2026 czerwony A5 + nowy wkład', 'B4', 0),
    ('ZEST-27-CZ-A5', 'Zestaw prezentowy: notes i kalendarz 2027 czerwony A5', 'B4', 0),
)


def build_plan(profile, scope_ids):
    if profile not in ('metal', 'stationery') or not isinstance(scope_ids, dict):
        raise ValueError('Wybierz profil metal lub stationery')
    try:
        scope = {key: str(uuid.UUID(scope_ids[key])) for key in SCOPE_KEYS}
    except (KeyError, ValueError, TypeError, AttributeError) as error:
        raise ValueError('Nieprawidłowy zakres instalacji') from error
    if any(scope[key] != scope_ids[key] or uuid.UUID(scope[key]).int == 0 for key in SCOPE_KEYS):
        raise ValueError('Nieprawidłowy zakres instalacji')
    if len(set(scope.values())) != len(SCOPE_KEYS):
        raise ValueError('Zakresy i adresy muszą mieć oddzielne identyfikatory')
    namespace = uuid.UUID(scope['instance_id'])
    uid = lambda name: str(uuid.uuid5(namespace, 'wms-industry-v1/' + profile + '/' + name))
    org, wh, owner = scope['organization_id'], scope['warehouse_id'], scope['owner_id']
    company = 'METALFORM' if profile == 'metal' else 'PAPIERNIK'
    products = METAL if profile == 'metal' else STATIONERY
    items = [dict(id=uid('item/'+sku), organization_id=org, sku=sku, name=name, unit='szt.', quantity_scale=0)
             for sku, name, _, _ in products]
    positions = [dict(id=uid('position/'+letter+str(level)), organization_id=org, warehouse_id=wh,
                      code=letter+str(level), kind='storage', name='Regał '+letter+' · poziom '+str(level))
                 for letter in 'AB' for level in range(1,5)]
    positions += [dict(id=scope['position_receiving_id'], organization_id=org, warehouse_id=wh,
                       code='PRZYJECIE-01', kind='receiving', name='Strefa przyjęć'),
                  dict(id=uid('position/WYDANIE-01'), organization_id=org, warehouse_id=wh,
                       code='WYDANIE-01', kind='shipping', name='Strefa wydań')]
    by_sku = {row['sku']: row for row in items}
    by_code = {row['code']: row for row in positions}
    recipes = []

    def recipe(sku, name, components, steps):
        recipes.append(dict(id=uid('recipe/'+sku), organization_id=org, warehouse_id=wh,
                            product_item_id=by_sku[sku]['id'], name=name,
                            components=[dict(item_id=by_sku[key]['id'], quantity=str(qty)) for key,qty in components],
                            steps=steps))

    if profile == 'metal':
        recipe('SZAFA-1800', 'Szafa metalowa 1800 × 800 × 400 mm',
               [('SR-M8X40',24), ('NAK-M8',24), ('POD-M8',48), ('NIT-4X10',32),
                ('ST-1800X800X15',2), ('ST-1800X400X15',2), ('ST-800X400X15',3),
                ('ZAW-60',4), ('ZAM-SZAFA',1), ('NOZ-REG',4), ('LAK-RAL7035',1)],
               ['Spawanie','Szlifowanie','Lakierowanie','Montaż'])
        recipe('SZAFKA-900', 'Szafka metalowa 900 × 600 × 400 mm',
               [('SR-M6X20',16),('NAK-M6',16),('POD-M6',32),('NIT-4X10',16),
                ('AL-500X300X2',4),('ZAW-60',2),('ZAM-SZAFA',1),('NOZ-REG',4)],
               ['Przygotowanie formatek','Spawanie','Szlifowanie','Lakierowanie','Montaż'])
    else:
        recipe('ZEST-26-CZ-A5', 'Odnowienie notesu 2026 czerwonego A5 · nowy wkład',
               [('NOT-26-CZ-A5',1),('WKL-A5-KR',1),('GUM-A5',1)], ['Kompletacja','Kontrola','Pakowanie'])
        recipe('ZEST-27-CZ-A5', 'Zestaw prezentowy 2027 czerwony A5',
               [('NOT-27-CZ-A5',1),('KAL-27-CZ-A5',1),('ETUI-A5',1)], ['Kompletacja','Kontrola','Pakowanie'])
    balances = [dict(id=uid('balance/'+item['sku']+'/'+position['code']), organization_id=org,
                     warehouse_id=wh, owner_id=owner, item_id=item['id'], position_id=position['id'],
                     stock_state='available', lot_id=None, serial_id=None)
                for item in items for position in positions]
    balance_ids = {(row['item_id'],row['position_id']): row['id'] for row in balances}
    opening = [dict(item_id=by_sku[sku]['id'], position_id=by_code[code]['id'],
                    balance_id=balance_ids[(by_sku[sku]['id'],by_code[code]['id'])], quantity=str(quantity))
               for sku, _, code, quantity in products if quantity > 0]
    samples=[]
    def sample(customer, ordered, recipe_index=None):
        index=len(samples)+1
        lines=[dict(item_id=by_sku[sku]['id'],quantity=str(quantity)) for sku,quantity in ordered]
        request=dict(organization_id=org,warehouse_id=wh,owner_id=owner,customer=customer,
                     reference='ZAM/PRZYKLAD/'+str(index).zfill(3),lines=lines)
        demand={}
        steps=[]
        recipe_id=''
        if recipe_index is not None:
            production=recipes[recipe_index]
            recipe_id=production['id']
            request['recipe_id']=recipe_id
            steps=production['steps'][:]
            for component in production['components']:
                demand[component['item_id']]=int(component['quantity'])*int(lines[0]['quantity'])
        else:
            for line in lines:
                demand[line['item_id']]=demand.get(line['item_id'],0)+int(line['quantity'])
        snapshot=dict(request,recipe_id=recipe_id,
                      requirements=[dict(item_id=item,quantity=str(demand[item])) for item in sorted(demand)],
                      steps=steps,request=request)
        samples.append(dict(command_id=uid('command/sample-order/'+str(index)),snapshot=snapshot))
    if profile=='metal':
        sample('ALFA Konstrukcje Sp. z o.o.',[('SZAFA-1800',10)],0)
        sample('NOVA Budownictwo Sp. z o.o.',[('SZAFKA-900',12)],1)
        sample('Warsztat Mechaniczny TOR Sp. z o.o.',[('SR-M6X20',300),('NIT-4X10',500),('AL-500X300X2',12)])
    else:
        sample('Biuro Projektowe Forma Sp. z o.o.',[('NOT-27-CZ-A5',40),('NOT-27-BI-A5',25),('NOT-27-CZ-A6',30),('KAL-27-CZ-A5',20),('KAL-27-SC-A3',15)])
        sample('Drukarnia Kolor Sp. z o.o.',[('ZEST-26-CZ-A5',50)],0)
        sample('Agencja Reklamowa Punkt Sp. z o.o.',[('ZEST-27-CZ-A5',60)],1)
    return dict(schema=1, profile=profile, company=company, scope=scope, actor_id=uid('actor/opening'),
                commands={action:uid('command/'+action) for action in ['create','activate','close']},
                movement_commands=[uid('command/receipt/'+str(index)) for index in range(len(opening))],
                opening=opening, balances=balances, sample_orders=samples,
                workspace=dict(organizations=[dict(id=org,name=company+' · przykład zakładu')],
                               warehouses=[dict(id=wh,organization_id=org,name=company+(' · magazyn materiałów i wyrobów' if profile=='metal' else ' · magazyn notesów i kalendarzy'))],
                               owners=[dict(id=owner,organization_id=org,name=company+' · towar własny')],
                               items=items,positions=positions,recipes=recipes))


def merge_workspace(current, plan):
    result = copy.deepcopy(current)
    for group, incoming in plan['workspace'].items():
        rows = result.setdefault(group, [])
        existing = {row['id']:row for row in rows}
        if len(existing) != len(rows):
            raise ValueError('Powtórzone identyfikatory katalogu')
        for row in incoming:
            old = existing.get(row['id'])
            if old is None:
                rows.append(copy.deepcopy(row))
            elif group in ('organizations','warehouses','owners'):
                if {key:value for key,value in old.items() if key!='name'} != {key:value for key,value in row.items() if key!='name'}:
                    raise ValueError('Nieoczekiwany zakres katalogu')
                old['name'] = row['name']
            elif group=='positions' and row['id']==plan['scope']['position_receiving_id']:
                if {key:value for key,value in old.items() if key!='name'} != {key:value for key,value in row.items() if key!='name'}:
                    raise ValueError('Nieoczekiwany adres przyjęcia')
                old['name'] = row['name']
            elif old != row:
                raise ValueError('Nieoczekiwane dane katalogu: '+group)
    namespace=uuid.UUID(plan['scope']['instance_id'])
    legacy_items={str(uuid.uuid5(namespace,'wms-workplace-v1/'+key)):sku
                  for key,sku in [('item_bolt','BOLT-M8'),('item_cable','CABLE-25')]}
    for row in result['items']:
        if row.get('organization_id')==plan['scope']['organization_id'] and legacy_items.get(row['id'])==row['sku']:
            row['archived']=True
    return result


def catalog_rows(plan):
    scope=plan['scope']; org=scope['organization_id']; wh=scope['warehouse_id']
    namespace=uuid.UUID(scope['instance_id'])
    uid=lambda name:str(uuid.uuid5(namespace,'wms-industry-v1/'+plan['profile']+'/'+name))
    unit='industry-'+scope['instance_id']+'-szt'
    rows=[('catalog.units',dict(code=unit,dimension='count'))]
    for item in plan['workspace']['items']:
        rows.append(('catalog.items',dict(id=item['id'],organization_id=org,sku=item['sku'],base_unit=unit,
                                         quantity_scale=item['quantity_scale'],tracking='none')))
    for position in plan['workspace']['positions']:
        location=scope['location_receiving_id'] if position['code']=='PRZYJECIE-01' else uid('location/'+position['code'])
        rows.append(('warehouse.locations',dict(id=location,organization_id=org,warehouse_id=wh,
                                               code=position['code'],kind=position['kind'],enabled=True)))
        rows.append(('warehouse.positions',dict(id=position['id'],organization_id=org,warehouse_id=wh,
                                               location_id=location,handling_unit_id=None)))
    rows += [('inventory.stock_balances',row) for row in plan['balances']]
    rows.append(('platform.actors',dict(id=plan['actor_id'],issuer='wms-industry-fixture',subject=scope['instance_id']+'/'+plan['profile']+'/v1')))
    return rows


def seed_sql(plan):
    if plan != build_plan(plan.get('profile'), plan.get('scope')):
        raise ValueError('Plan danych został zmieniony po przygotowaniu')
    literal=workplace.sql_literal
    scope=plan['scope']; org=scope['organization_id']; wh=scope['warehouse_id']; owner=scope['owner_id']; actor=plan['actor_id']
    chunks=['BEGIN; SET LOCAL standard_conforming_strings=on; SET LOCAL lock_timeout=\'2s\'; SET LOCAL statement_timeout=\'30s\';',
            'SELECT pg_advisory_xact_lock(hashtextextended('+literal('industry/'+scope['instance_id'])+',0));',
            'DO $industry$ BEGIN IF NOT EXISTS(SELECT FROM hardened_private.installation WHERE singleton AND id='+literal(scope['instance_id'])+") THEN RAISE EXCEPTION 'Niezgodna instalacja'; END IF; END $industry$;",
            'DO $industry$ BEGIN IF NOT EXISTS(SELECT FROM warehouse.warehouses w JOIN catalog.stock_owners o ON o.organization_id=w.organization_id WHERE w.organization_id='+literal(org)+' AND w.id='+literal(wh)+' AND o.id='+literal(owner)+") THEN RAISE EXCEPTION 'Niezgodny zakres magazynu'; END IF; END $industry$;"]
    for table,row in catalog_rows(plan):
        key='id' if 'id' in row else 'code'
        key_match=key+'='+literal(row[key]); matches=workplace.where(row)
        chunks.append('DO $industry$ BEGIN IF EXISTS(SELECT FROM '+table+' WHERE '+key_match+') AND NOT EXISTS(SELECT FROM '+table+' WHERE '+matches+") THEN RAISE EXCEPTION 'Nieoczekiwane dane: "+table+"'; END IF; END $industry$;")
        chunks.append('INSERT INTO '+table+'('+','.join(row)+') VALUES('+','.join(literal(value) for value in row.values())+') ON CONFLICT DO NOTHING;')
        chunks.append('DO $industry$ BEGIN IF NOT EXISTS(SELECT FROM '+table+' WHERE '+matches+") THEN RAISE EXCEPTION 'Nieoczekiwane dane: "+table+"'; END IF; END $industry$;")
    chunks.append('DO $industry$ BEGIN IF EXISTS(SELECT FROM platform.actor_grants WHERE actor_id='+literal(actor)+") THEN RAISE EXCEPTION 'Nieoczekiwane uprawnienia konta bilansu otwarcia'; END IF; END $industry$;")
    for permission in ['document.create','document.transition','movement.create']:
        chunks.append('INSERT INTO platform.actor_grants(actor_id,organization_id,warehouse_id,owner_id,permission) VALUES('+','.join(literal(value) for value in [actor,org,wh,owner,permission])+');')
    create=dict(organization_id=org,warehouse_id=wh,owner_id=owner,kind='receipt',number='PZ/BO/'+plan['company']+'/v1',
                lines=[dict(item_id=row['item_id'],quantity=row['quantity']) for row in plan['opening']])
    activate=dict(organization_id=org,warehouse_id=wh,expected_document_version=1,state='active')
    close=dict(organization_id=org,warehouse_id=wh,expected_document_version=2+len(plan['opening']),state='closed')
    js=lambda value:literal(json.dumps(value,ensure_ascii=False,separators=(',',':')))+'::jsonb'
    body=['DO $industry$ DECLARE result jsonb; document uuid; line uuid; BEGIN',
          'result:=warehouse.command_document('+literal(plan['commands']['create'])+','+literal(actor)+',NULL,'+js(create)+');',
          "document:=(result->>'document_id')::uuid;",
          'PERFORM warehouse.command_document('+literal(plan['commands']['activate'])+','+literal(actor)+',document,'+js(activate)+');']
    for index,row in enumerate(plan['opening']):
        body += ['SELECT id INTO STRICT line FROM warehouse.document_lines WHERE document_id=document AND line_no='+str(index+1)+';',
                 'PERFORM inventory.post_movement('+literal(plan['movement_commands'][index])+','+literal(actor)+',line,'+str(index+2)+",'receipt',NULL,"+literal(row['balance_id'])+','+row['quantity']+',NULL,\'Bilans otwarcia — dane przykładowe '+plan['company']+'\');']
    body += ['PERFORM warehouse.command_document('+literal(plan['commands']['close'])+','+literal(actor)+',document,'+js(close)+');', 'END $industry$;']
    chunks += body
    for order in plan['sample_orders']:
        chunks.append('SELECT warehouse.command_work_order('+literal(order['command_id'])+','+literal(actor)+','+js(order['snapshot'])+');')
    chunks.append('DELETE FROM platform.actor_grants WHERE actor_id='+literal(actor)+';')
    chunks.append('COMMIT;')
    return '\n'.join(chunks)+'\n'


def add_employee_permissions_sql(state, plan):
    """Only add permissions requested for the existing warehouse employee."""
    literal=workplace.sql_literal; scope=plan['scope']
    actor=str(uuid.UUID(state['ids']['actor_magazynier']))
    subject=str(uuid.UUID(state['users']['magazynier']['subject']))
    statement=['BEGIN;', 'DO $industry$ BEGIN IF NOT EXISTS(SELECT FROM platform.actors WHERE id='+literal(actor)+' AND subject='+literal(subject)+") THEN RAISE EXCEPTION 'Niezgodny magazynier'; END IF; END $industry$;"]
    for permission in ['document.issue','document.move','reservation.create','reservation.read']:
        statement.append('INSERT INTO platform.actor_grants(actor_id,organization_id,warehouse_id,owner_id,permission) VALUES('+','.join(literal(value) for value in [actor,scope['organization_id'],scope['warehouse_id'],scope['owner_id'],permission])+') ON CONFLICT DO NOTHING;')
    statement.append('COMMIT;')
    return '\n'.join(statement)+'\n'


class Runtime:
    sql=workplace.Runtime.sql
    publish_workspace=workplace.Runtime.publish_workspace


def apply(runtime, profile, identity, state, workspace, expected_instance):
    legacy=workplace.build_plan(identity,expected_instance)
    workplace.validate_state(state,legacy)
    if any(state.get('ids',{}).get(key)!=value for key,value in legacy['ids'].items()):
        raise ValueError('Niezgodne identyfikatory stanowiska')
    if workspace.get('oidc')!=legacy['workspace']['oidc']:
        raise ValueError('Niezgodny dostawca tożsamości stanowiska')
    ids=state['ids']
    scope={key+'_id':ids[key] for key in ['organization','warehouse','owner']}
    scope.update(instance_id=state['instance_id'],position_receiving_id=ids['position_receiving'],location_receiving_id=ids['location_receiving'])
    plan=build_plan(profile,scope)
    merged=merge_workspace(workspace,plan)
    runtime.sql(seed_sql(plan))
    runtime.sql(add_employee_permissions_sql(state,plan))
    runtime.publish_workspace(workplace.WORKSPACE_CONFIG,merged)
    return dict(status='configured',profile=profile,instance_id=state['instance_id'],company=plan['company'],
                products=len(plan['workspace']['items']),positions=len(plan['workspace']['positions']),
                opening_lines=len(plan['opening']),recipes=plan['workspace']['recipes'],
                sample_orders=len(plan['sample_orders']),order_references=[row['snapshot']['reference'] for row in plan['sample_orders']],
                stock_replay_preserves_later_activity=True,credentials_changed=False)


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action',choices=['plan','apply'])
    parser.add_argument('--lab',action='store_true',required=True)
    parser.add_argument('--profile',choices=['metal','stationery'],required=True)
    parser.add_argument('--expected-instance-id',required=True)
    parser.add_argument('--state',type=Path,default=Path('/etc/wms/workplace-users.json'))
    parser.add_argument('--config',type=Path,default=Path('/etc/wms'))
    args=parser.parse_args()
    if os.name!='posix' or os.geteuid()!=0:
        parser.error('Uruchom przez sudo na wybranej maszynie laboratoryjnej')
    identity=json.loads((args.config/'identity.json').read_text())
    state=workplace.read_protected(args.state)
    workspace=json.loads(workplace.WORKSPACE_CONFIG.read_text())
    legacy=workplace.build_plan(identity,args.expected_instance_id)
    scope={key+'_id':state['ids'][key] for key in ['organization','warehouse','owner']}
    scope.update(instance_id=state['instance_id'],position_receiving_id=state['ids']['position_receiving'],location_receiving_id=state['ids']['location_receiving'])
    if state['instance_id']!=legacy['instance_id']:
        parser.error('Niezgodna instalacja danych dostępu')
    if args.action=='plan':
        print(json.dumps(build_plan(args.profile,scope),ensure_ascii=False,indent=2))
    else:
        print(json.dumps(apply(Runtime(),args.profile,identity,state,workspace,args.expected_instance_id),ensure_ascii=False))


if __name__=='__main__':
    main()
