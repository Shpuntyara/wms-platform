"""Configure the explicitly selected synthetic laboratory employee workplace.

Creates no database logins and never stores service credentials in the public
workspace. A temporary administrator bearer token is supplied in a protected
file by the operator; this tool never bootstraps or prints that credential.
"""
import argparse
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import secrets
import ssl
import stat
import subprocess
import sys
import urllib.error
import urllib.parse
import urllib.request
import uuid

WORKSPACE_CONFIG = Path('/etc/wms-workplace/workspace.json')
PERMISSIONS = {'biuro': ('document.read', 'document.create', 'document.transition', 'stock.read'),
               'magazynier': ('document.read', 'document.transition', 'stock.read', 'movement.create')}

def build_plan(identity, expected_instance):
    try:
        installation = str(uuid.UUID(identity['instance_id']))
        expected = str(uuid.UUID(expected_instance))
        address = str(ipaddress.IPv4Address(identity['address']))
        parsed = urllib.parse.urlsplit(identity['issuer'])
        audience = identity['audience']
    except (ValueError, TypeError, KeyError) as error:
        raise ValueError('Nieprawidłowa tożsamość instalacji') from error
    if (installation != expected or installation != identity['instance_id']
            or ipaddress.ip_address(address).is_loopback or ipaddress.ip_address(address).is_link_local
            or ipaddress.ip_address(address).is_unspecified or ipaddress.ip_address(address).is_multicast
            or parsed.scheme != 'https' or parsed.netloc != address or parsed.query or parsed.fragment
            or not parsed.path.startswith('/auth/realms/') or parsed.path.endswith('/')
            or '/' in parsed.path.removeprefix('/auth/realms/') or not audience):
        raise ValueError('Tożsamość lub adres instalacji nie zgadza się z planem')
    namespace = uuid.UUID(installation)
    names = ['organization','warehouse','owner','item_bolt','item_cable','location_receiving','location_storage',
             'position_receiving','position_storage','balance_bolt_receiving','balance_bolt_storage',
             'balance_cable_receiving','balance_cable_storage','actor_biuro','actor_magazynier']
    ids = {name: str(uuid.uuid5(namespace, 'wms-workplace-v1/'+name)) for name in names}
    suffix = installation[:8]
    origin = 'https://'+address
    realm = parsed.path.removeprefix('/auth/realms/')
    scope = {'organization_id':ids['organization'],'warehouse_id':ids['warehouse']}
    workspace = {
        'oidc':{'url':origin+'/auth','realm':realm,'clientId':'wms-browser'},
        'organizations':[{'id':ids['organization'],'name':'Firma testowa '+suffix}],
        'warehouses':[{'id':ids['warehouse'],'organization_id':ids['organization'],'name':'Magazyn '+suffix}],
        'owners':[{'id':ids['owner'],'organization_id':ids['organization'],'name':'Właściciel główny'}],
        'items':[
            {'id':ids['item_bolt'],'organization_id':ids['organization'],'sku':'BOLT-M8','name':'Śruba M8 × 40, ocynkowana','unit':'szt.','quantity_scale':0},
            {'id':ids['item_cable'],'organization_id':ids['organization'],'sku':'CABLE-25','name':'Przewód zasilający 3 × 2,5','unit':'m','quantity_scale':3}],
        'positions':[
            {'id':ids['position_receiving'],**scope,'code':'PRZYJECIE-01','kind':'receiving'},
            {'id':ids['position_storage'],**scope,'code':'A-01-02','kind':'storage'}]}
    client = {'clientId':'wms-browser','name':'Stanowisko WMS','enabled':True,'protocol':'openid-connect',
        'publicClient':True,'standardFlowEnabled':True,'implicitFlowEnabled':False,
        'directAccessGrantsEnabled':False,'serviceAccountsEnabled':False,
        'redirectUris':[origin+'/app/'],'webOrigins':[origin],
        'attributes':{'pkce.code.challenge.method':'S256','post.logout.redirect.uris':origin+'/app/','wms.workplace.installation':installation},
        'protocolMappers':[{'name':'wms-audience','protocol':'openid-connect','protocolMapper':'oidc-audience-mapper',
            'consentRequired':False,'config':{'included.client.audience':audience,'id.token.claim':'false','access.token.claim':'true','userinfo.token.claim':'false'}}]}
    balances = [{'id':ids['balance_'+item+'_'+position],**scope,'owner_id':ids['owner'],'item_id':ids['item_'+item],
                 'position_id':ids['position_'+position],'stock_state':'available','lot_id':None,'serial_id':None}
                for item in ['bolt','cable'] for position in ['receiving','storage']]
    return {'schema':1,'instance_id':installation,'origin':origin,'issuer':identity['issuer'],'audience':audience,
            'realm':realm,'ids':ids,'workspace':workspace,'client':client,'balances':balances,
            'workspace_path':str(WORKSPACE_CONFIG).replace('\\','/'),
            'permissions':{user:list(permissions) for user,permissions in PERMISSIONS.items()}}


def sql_literal(value):
    if value is None: return 'NULL'
    if isinstance(value, bool): return 'true' if value else 'false'
    if isinstance(value, int): return str(value)
    if not isinstance(value,str) or '\x00' in value:
        raise ValueError('Nieprawidłowa wartość SQL')
    return "'"+value.replace("'","''")+"'"


def catalog_rows(plan, subjects):
    ids, config = plan['ids'], plan['workspace']
    scope = {'organization_id':ids['organization'],'warehouse_id':ids['warehouse']}
    code = 'workplace-'+plan['instance_id']
    rows = [
        ('platform.organizations',{'id':ids['organization'],'code':code,'name':config['organizations'][0]['name']}),
        ('warehouse.warehouses',{'id':ids['warehouse'],'organization_id':ids['organization'],'code':code,'timezone':'Europe/Warsaw'}),
        ('catalog.stock_owners',{'id':ids['owner'],'organization_id':ids['organization'],'code':code}),
        ('catalog.units',{'code':code+'-szt','dimension':'count'}),
        ('catalog.units',{'code':code+'-m','dimension':'length'})]
    for item, unit in zip(config['items'],['szt','m']):
        rows.append(('catalog.items',{'id':item['id'],'organization_id':ids['organization'],'sku':item['sku'],
                     'base_unit':code+'-'+unit,'quantity_scale':item['quantity_scale'],'tracking':'none'}))
    for position, name in zip(config['positions'],['receiving','storage']):
        rows.append(('warehouse.locations',{'id':ids['location_'+name],**scope,'code':position['code'],'kind':position['kind'],'enabled':True}))
        rows.append(('warehouse.positions',{'id':position['id'],**scope,'location_id':ids['location_'+name],'handling_unit_id':None}))
    rows.extend(('inventory.stock_balances',balance) for balance in plan['balances'])
    for username, subject in (subjects or {}).items():
        if username not in PERMISSIONS: raise ValueError('Nieznany użytkownik')
        subject = str(uuid.UUID(subject))
        rows.append(('platform.actors',{'id':ids['actor_'+username],'issuer':plan['issuer'],'subject':subject}))
    return rows


def where(row):
    return ' AND '.join(name+' IS NOT DISTINCT FROM '+sql_literal(value) for name,value in row.items())

def seed_sql(plan, subjects=None, *, preflight=False):
    # Table/column names below are generated only by this module; data is quoted
    # with standard_conforming_strings fixed inside the same transaction.
    chunks = ["BEGIN; SET LOCAL standard_conforming_strings=on; SET LOCAL lock_timeout='2s';",
              "SELECT pg_advisory_xact_lock(hashtextextended("+sql_literal('workplace/'+plan['instance_id'])+",0));",
              "DO $workplace$ BEGIN IF NOT EXISTS(SELECT FROM hardened_private.installation WHERE singleton AND id="+sql_literal(plan['instance_id'])+") THEN RAISE EXCEPTION 'Niezgodna instalacja'; END IF; END $workplace$;"]
    for table,row in catalog_rows(plan,subjects):
        key = 'id' if 'id' in row else 'code'
        matches = where(row)
        key_match = key+'='+sql_literal(row[key])
        chunks.append("DO $workplace$ BEGIN IF EXISTS(SELECT FROM "+table+" WHERE "+key_match+") AND NOT EXISTS(SELECT FROM "+table+" WHERE "+matches+") THEN RAISE EXCEPTION 'Nieoczekiwane dane: "+table+"'; END IF; END $workplace$;")
        if not preflight:
            chunks.append('INSERT INTO '+table+'('+','.join(row)+') VALUES('+','.join(sql_literal(value) for value in row.values())+') ON CONFLICT DO NOTHING;')
            chunks.append("DO $workplace$ BEGIN IF NOT EXISTS(SELECT FROM "+table+" WHERE "+matches+") THEN RAISE EXCEPTION 'Nieoczekiwane dane: "+table+"'; END IF; END $workplace$;")
    if subjects:
        expected = []
        for username in subjects:
            for permission in PERMISSIONS[username]:
                expected.append({'actor_id':plan['ids']['actor_'+username], 'organization_id':plan['ids']['organization'],
                                 'warehouse_id':plan['ids']['warehouse'],'owner_id':plan['ids']['owner'],'permission':permission})
        actors = ','.join(sql_literal(plan['ids']['actor_'+name]) for name in subjects)
        allowed = ' OR '.join('('+where(row)+')' for row in expected)
        chunks.append("DO $workplace$ BEGIN IF EXISTS(SELECT FROM platform.actor_grants WHERE actor_id IN("+actors+") AND NOT("+allowed+")) THEN RAISE EXCEPTION 'Nieoczekiwane uprawnienia użytkownika'; END IF; END $workplace$;")
        if not preflight:
            for row in expected:
                chunks.append('INSERT INTO platform.actor_grants('+','.join(row)+') VALUES('+','.join(sql_literal(value) for value in row.values())+') ON CONFLICT DO NOTHING;')
    chunks.append('COMMIT;')
    return '\n'.join(chunks)+'\n'

def read_protected(path):
    path = Path(path)
    info = path.lstat()
    if (not stat.S_ISREG(info.st_mode) or info.st_size > 256*1024
            or os.name == 'posix' and (info.st_uid != os.geteuid() or info.st_mode & 0o077)):
        raise ValueError('Plik prywatny ma nieprawidłowy typ lub uprawnienia')
    return json.loads(path.read_text(encoding='utf8'))

def write_protected(path, value):
    path = Path(path)
    path.parent.mkdir(parents=True,exist_ok=True,mode=0o700)
    if path.exists() or path.is_symlink(): read_protected(path)
    temporary = path.with_name(path.name+'.new-'+secrets.token_hex(6))
    fd = os.open(temporary,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
    try:
        with os.fdopen(fd,'w',encoding='utf8',newline='\n') as stream:
            json.dump(value,stream,ensure_ascii=False,indent=2)
            stream.write('\n'); stream.flush(); os.fsync(stream.fileno())
        os.replace(temporary,path)
        os.chmod(path,0o600)
    finally:
        temporary.unlink(missing_ok=True)


def require_response(runtime,method,path,data=None,expected=200):
    status, body = runtime.admin(method,path,data)
    if status != expected: raise ValueError('Dostawca tożsamości odrzucił konfigurację stanowiska')
    return body


def user_representation(plan,username,password):
    return {'username':username,'enabled':True,'firstName':'Biuro' if username=='biuro' else 'Magazynier',
            'lastName':'Testowy','email':username+'@wms.invalid','emailVerified':True,'requiredActions':[],
            'attributes':{'wms.workplace.installation':[plan['instance_id']]},
            'credentials':[{'type':'password','temporary':False,'value':password}]}


def validate_state(state,plan):
    if state.get('schema') != 1 or state.get('instance_id') != plan['instance_id'] or state.get('origin') != plan['origin'] or set(state.get('users',{})) != set(PERMISSIONS):
        raise ValueError('Prywatny zapis stanowiska należy do innej instalacji')
    for username,user in state['users'].items():
        if user.get('username') != username or not isinstance(user.get('password'),str) or len(user['password']) < 24:
            raise ValueError('Prywatny zapis użytkownika jest nieprawidłowy')
        if user.get('subject') is not None: uuid.UUID(user['subject'])


def preflight_keycloak(runtime,plan,state):
    realm = require_response(runtime,'GET','')
    if realm.get('realm') != plan['realm'] or not realm.get('enabled'):
        raise ValueError('Niezgodny realm instalacji')
    profile = require_response(runtime,'GET','/users/profile')
    if not isinstance(profile,dict) or not isinstance(profile.get('attributes'),list):
        raise ValueError('Nieprawidłowy profil użytkownika')
    marker = {'name':'wms.workplace.installation','displayName':'Identyfikator stanowiska WMS','multivalued':False,
              'permissions':{'view':['admin'],'edit':['admin']},'validations':{'length':{'min':36,'max':36}}}
    existing = [entry for entry in profile['attributes'] if entry.get('name') == marker['name']]
    if len(existing)>1 or existing and any(existing[0].get(key) != value for key,value in marker.items()):
        raise ValueError('Niezgodna definicja atrybutu właściciela stanowiska')
    desired_profile = dict(profile,attributes=profile['attributes']+[marker]) if not existing else profile
    clients = require_response(runtime,'GET','/clients?clientId=wms-browser')
    if not isinstance(clients,list) or len(clients) > 1: raise ValueError('Niejednoznaczny klient przeglądarkowy')
    if clients:
        client = clients[0]
        if client.get('attributes',{}).get('wms.workplace.installation') != plan['instance_id']:
            raise ValueError('Klient przeglądarkowy nie należy do tego stanowiska')
        for field in ['clientId','publicClient','standardFlowEnabled','implicitFlowEnabled','directAccessGrantsEnabled','serviceAccountsEnabled','redirectUris','webOrigins']:
            if client.get(field) != plan['client'][field]: raise ValueError('Konfiguracja klienta przeglądarkowego uległa zmianie')
        if client.get('attributes',{}).get('pkce.code.challenge.method') != 'S256':
            raise ValueError('Klient wymaga PKCE S256')
        if client.get('attributes',{}).get('post.logout.redirect.uris') != plan['origin']+'/app/':
            raise ValueError('Niezgodny adres powrotu po wylogowaniu')
        if client.get('protocolMappers') != plan['client']['protocolMappers']:
            # Keycloak adds mapper IDs on read; compare immutable meaning.
            mappers = [{key:value for key,value in mapper.items() if key != 'id'} for mapper in client.get('protocolMappers',[])]
            if mappers != plan['client']['protocolMappers']: raise ValueError('Niezgodna konfiguracja audience klienta')
    users = {}
    for username in PERMISSIONS:
        found = require_response(runtime,'GET','/users?username='+username+'&exact=true')
        if not isinstance(found,list) or len(found) > 1: raise ValueError('Niejednoznaczny użytkownik stanowiska')
        if found:
            user = found[0]
            owned = user.get('attributes',{}).get('wms.workplace.installation')
            if owned != [plan['instance_id']] or not state or not user.get('enabled'):
                raise ValueError('Istniejący użytkownik nie należy do tego stanowiska')
            subject = str(uuid.UUID(user['id']))
            saved = state['users'][username].get('subject')
            if saved and saved != subject: raise ValueError('Tożsamość użytkownika uległa zmianie')
            users[username] = subject
        elif state and state['users'][username].get('subject'):
            raise ValueError('Wcześniej skonfigurowany użytkownik został usunięty')
    return realm,clients,users,profile,desired_profile

def apply(runtime, expected_instance, *, lab=False, credentials_file=None, workspace_file=None):
    if not lab: raise ValueError('Tworzenie danych wymaga jawnego parametru --lab')
    plan = build_plan(runtime.identity,expected_instance)
    actual = runtime.sql('SELECT id::text FROM hardened_private.installation WHERE singleton;')
    if actual != expected_instance: raise ValueError('Baza danych należy do innej instalacji')
    credentials_file = Path(credentials_file or '/etc/wms/workplace-users.json')
    workspace_file = Path(workspace_file or WORKSPACE_CONFIG)
    state = read_protected(credentials_file) if credentials_file.exists() or credentials_file.is_symlink() else None
    if state: validate_state(state,plan)
    realm,clients,subjects,profile,desired_profile = preflight_keycloak(runtime,plan,state)
    runtime.sql(seed_sql(plan,subjects,preflight=True))
    if state is None:
        state = {'schema':1,'instance_id':plan['instance_id'],'origin':plan['origin'],
                 'users':{username:{'username':username,'password':secrets.token_urlsafe(24),'subject':None} for username in PERMISSIONS},
                 'catalog':{'organization_id':plan['ids']['organization'],'warehouse_id':plan['ids']['warehouse'],
                     'owner_id':plan['ids']['owner'],'item_id':plan['ids']['item_bolt'],'sku':'BOLT-M8',
                     'receiving_code':'PRZYJECIE-01','storage_code':'A-01-02'},'ids':plan['ids']}
        write_protected(credentials_file,state)
    if desired_profile != profile: require_response(runtime,'PUT','/users/profile',desired_profile,200)
    if not clients: require_response(runtime,'POST','/clients',plan['client'],201)
    for username in PERMISSIONS:
        if username not in subjects:
            require_response(runtime,'POST','/users',user_representation(plan,username,state['users'][username]['password']),201)
            found = require_response(runtime,'GET','/users?username='+username+'&exact=true')
            if len(found) != 1 or found[0].get('attributes',{}).get('wms.workplace.installation') != [plan['instance_id']]:
                raise ValueError('Nie udało się potwierdzić nowego użytkownika')
            subjects[username] = str(uuid.UUID(found[0]['id']))
        if state['users'][username].get('subject') != subjects[username]:
            state['users'][username]['subject'] = subjects[username]
            write_protected(credentials_file,state)
    desired_realm = dict(realm,internationalizationEnabled=True,defaultLocale='pl',supportedLocales=sorted(set(realm.get('supportedLocales',[]))|{'pl'}))
    if desired_realm != realm: require_response(runtime,'PUT','',desired_realm,204)
    runtime.sql(seed_sql(plan,subjects))
    runtime.publish_workspace(workspace_file,plan['workspace'])
    return {'schema':1,'status':'configured','instance_id':plan['instance_id'],'origin':plan['origin'],
            'subjects':subjects,'catalog':state['catalog'],'workspace_path':str(workspace_file),
            'workspace_sha256':hashlib.sha256(json.dumps(plan['workspace'],sort_keys=True,ensure_ascii=False).encode()).hexdigest(),
            'permissions':plan['permissions']}


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self,*args): return None


class Runtime:
    def __init__(self,config,token_file):
        self.config = Path(config)
        self.identity = json.loads((self.config/'identity.json').read_text())
        protected = read_protected(self.config/'secrets.json')
        if protected['instance_id'] != self.identity['instance_id']: raise ValueError('Niezgodna tożsamość sekretów')
        token = read_protected(token_file).get('access_token')
        if not isinstance(token,str) or not token or len(token) > 32768: raise ValueError('Nieprawidłowy token administracyjny')
        self.token = token
        self.origin = 'https://'+str(ipaddress.IPv4Address(self.identity['address']))
        context = ssl.create_default_context(cafile=str(self.config/'tls/ca.crt'))
        self.opener = urllib.request.build_opener(urllib.request.HTTPSHandler(context=context),urllib.request.ProxyHandler({}),NoRedirect())
        self.realm = urllib.parse.urlsplit(self.identity['issuer']).path.removeprefix('/auth/realms/')

    def admin(self,method,path,data=None):
        if (path and not path.startswith('/')) or '\\' in path or '..' in path or '://' in path:
            raise ValueError('Nieprawidłowa ścieżka administracyjna')
        url = self.origin+'/auth/admin/realms/'+urllib.parse.quote(self.realm,safe='')+path
        headers = {'Authorization':'Bearer '+self.token}
        if data is not None: headers['Content-Type']='application/json'
        request = urllib.request.Request(url,method=method,headers=headers,data=None if data is None else json.dumps(data).encode())
        try: response = self.opener.open(request,timeout=30)
        except urllib.error.HTTPError as error: response = error
        except Exception as error: raise ValueError('Połączenie z dostawcą tożsamości nie powiodło się') from error
        with response:
            raw = response.read(2*1024*1024+1)
            if len(raw) > 2*1024*1024: raise ValueError('Odpowiedź administracyjna przekracza limit')
            try: body = json.loads(raw) if raw else {}
            except (ValueError,UnicodeDecodeError): body = {}
            return response.code,body

    def sql(self,statement):
        result = subprocess.run(['docker','exec','-i','wms-postgres','psql','-X','-q','-At','-v','ON_ERROR_STOP=1','-U','wms_admin','-d','wms'],
                                input=statement,text=True,encoding='utf8',capture_output=True,timeout=45)
        if result.returncode != 0: raise ValueError('Nie udało się skonfigurować lub potwierdzić danych stanowiska')
        return result.stdout.strip()

    def publish_workspace(self,path,config):
        import grp
        path = Path(path)
        if path != WORKSPACE_CONFIG: raise ValueError('Nieprawidłowa ścieżka konfiguracji stanowiska')
        path.parent.mkdir(parents=True,exist_ok=True,mode=0o750)
        if path.parent.is_symlink(): raise ValueError('Katalog konfiguracji nie może być dowiązaniem')
        group = grp.getgrnam('wms').gr_gid
        os.chown(path.parent,0,group); os.chmod(path.parent,0o750)
        if path.exists() and not stat.S_ISREG(path.lstat().st_mode): raise ValueError('Nieprawidłowy plik konfiguracji stanowiska')
        temporary = path.with_name('workspace.new-'+secrets.token_hex(6))
        fd = os.open(temporary,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o640)
        try:
            with os.fdopen(fd,'w',encoding='utf8',newline='\n') as stream:
                json.dump(config,stream,ensure_ascii=False,indent=2); stream.write('\n'); stream.flush(); os.fsync(stream.fileno())
            os.chown(temporary,0,group); os.chmod(temporary,0o640)
            os.replace(temporary,path)
        finally: temporary.unlink(missing_ok=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action',choices=['plan','apply'])
    parser.add_argument('--expected-instance-id',required=True)
    parser.add_argument('--config',type=Path,default=Path('/etc/wms'))
    parser.add_argument('--admin-token-file',type=Path)
    parser.add_argument('--lab',action='store_true')
    args = parser.parse_args()
    if os.name != 'posix' or os.geteuid() != 0: raise ValueError('Uruchom narzędzie jako root wewnątrz wskazanej maszyny laboratoryjnej')
    if args.action == 'plan':
        result = build_plan(json.loads((args.config/'identity.json').read_text()),args.expected_instance_id)
    else:
        import fcntl
        if not args.lab or not args.admin_token_file: raise ValueError('Wymagane --lab oraz --admin-token-file')
        lockpath = Path('/run/wms-workplace.lock')
        descriptor = os.open(lockpath,os.O_CREAT|os.O_WRONLY|getattr(os,'O_NOFOLLOW',0),0o600)
        with os.fdopen(descriptor,'w') as lock:
            fcntl.flock(lock,fcntl.LOCK_EX|fcntl.LOCK_NB)
            result = apply(Runtime(args.config,args.admin_token_file),args.expected_instance_id,lab=True,
                           credentials_file=args.config/'workplace-users.json')
    print(json.dumps(result,ensure_ascii=False,sort_keys=True))


if __name__ == '__main__':
    try: main()
    except Exception:
        print('Konfiguracja stanowiska nie została ukończona. Sprawdź prywatne dane i stan wskazanej instalacji.',file=sys.stderr)
        raise SystemExit(1)
