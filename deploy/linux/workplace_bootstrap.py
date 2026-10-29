"""Bounded official Keycloak temporary administrator bootstrap for a WMS lab."""
import argparse
import importlib.util
import json
import os
from pathlib import Path
import re
import secrets
import ssl
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid

_spec = importlib.util.spec_from_file_location('wms_workplace_sdk',Path(__file__).with_name('workplace.py'))
workplace = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(workplace)


def _owned_state(state,instance):
    if (state.get('instance_id') != instance or not re.fullmatch(r'wms-workplace-admin-[a-f0-9]{32}',state.get('client_id',''))
            or state.get('status') not in ('pending','cleanup_failed','cleaned')
            or state.get('status') != 'cleaned' and (not isinstance(state.get('secret'),str) or len(state['secret'])<32)):
        raise ValueError('Niezgodny prywatny zapis tymczasowego administratora')


def _remove_token(token_file,state):
    if token_file.exists() or token_file.is_symlink():
        token = workplace.read_protected(token_file)
        if token.get('instance_id') != state['instance_id'] or token.get('client_id') != state['client_id']:
            raise ValueError('Plik tokena nie należy do tej operacji')
        token_file.unlink()


def _cleanup(runtime,state,state_file,token_file,tokens=None):
    try:
        if runtime.client_exists(state['client_id']):
            tokens = tokens or runtime.admin_token(state['client_id'],state['secret'])
            runtime.cleanup_client(state,tokens)
            if runtime.client_exists(state['client_id']): raise ValueError('Tymczasowy administrator nadal istnieje')
        _remove_token(token_file,state)
    except Exception:
        state['status']='cleanup_failed'
        workplace.write_protected(state_file,state)
        try: _remove_token(token_file,state)
        except Exception: pass
        raise ValueError('Nie udało się usunąć tymczasowego administratora; zachowano prywatny zapis odzyskiwania') from None
    state['status']='cleaned'
    state.pop('secret',None)
    workplace.write_protected(state_file,state)


def provision(runtime, expected_instance, *, lab=False, state_file=None, token_file=None, cleanup_only=False):
    if not lab: raise ValueError('Wymagane jawne --lab')
    plan=workplace.build_plan(runtime.identity,expected_instance)
    if runtime.sql('SELECT id::text FROM hardened_private.installation WHERE singleton;') != expected_instance:
        raise ValueError('Niezgodna instalacja bazy danych')
    state_file=Path(state_file or '/etc/wms/workplace-bootstrap-state.json')
    token_file=Path(token_file or '/run/wms-workplace-admin.json')
    previous=workplace.read_protected(state_file) if state_file.exists() or state_file.is_symlink() else None
    if previous:
        _owned_state(previous,expected_instance)
        if previous['status'] != 'cleaned':
            if runtime.client_exists(previous['client_id']) and not cleanup_only:
                raise ValueError('Najpierw uzgodnij poprzednie tymczasowe konto poleceniem cleanup')
            if cleanup_only:
                runtime.compose('up','-d','keycloak'); runtime.discovery()
            _cleanup(runtime,previous,state_file,token_file)
        else: _remove_token(token_file,previous)
    elif token_file.exists() or token_file.is_symlink():
        raise ValueError('Istniejący plik tokena nie ma zapisu własności')
    if cleanup_only: return {'status':'cleaned','instance_id':expected_instance}
    state={'schema':1,'instance_id':expected_instance,'client_id':'wms-workplace-admin-'+uuid.uuid4().hex,
           'secret':secrets.token_urlsafe(32),'status':'pending'}
    workplace.write_protected(state_file,state)
    result=None; error=None; tokens=None
    try:
        try:
            runtime.compose('stop','keycloak')
            runtime.bootstrap(state['client_id'],state['secret'])
        finally:
            runtime.compose('up','-d','keycloak')
            runtime.discovery()
        tokens=runtime.admin_token(state['client_id'],state['secret'])
        workplace.write_protected(token_file,dict(tokens,instance_id=expected_instance,client_id=state['client_id']))
        result=runtime.apply(token_file,expected_instance)
    except Exception:
        error=ValueError('Konfiguracja stanowiska nie została ukończona')
    finally:
        try: _cleanup(runtime,state,state_file,token_file,tokens)
        except ValueError as cleanup_error: error=cleanup_error
    if error: raise error from None
    return dict(result,temporary_admin_removed=True)

def bootstrap_command(config, client_id, secret):
    command=['docker','compose','--project-name','wms','--file',str(Path(config)/'compose.yaml'),
             'run','--rm','--no-deps','-e','WMS_WORKPLACE_ADMIN_SECRET','keycloak','bootstrap-admin','service',
             '--client-id',client_id,'--client-secret:env=WMS_WORKPLACE_ADMIN_SECRET','--no-prompt']
    environment=dict(os.environ,WMS_WORKPLACE_ADMIN_SECRET=secret)
    return command,environment


class Runtime:
    def __init__(self,config):
        self.config=Path(config)
        self.identity=json.loads((self.config/'identity.json').read_text())
        if workplace.read_protected(self.config/'secrets.json')['instance_id'] != self.identity['instance_id']:
            raise ValueError('Niezgodna instalacja sekretów')
        self.origin='https://'+self.identity['address']
        context=ssl.create_default_context(cafile=str(self.config/'tls/ca.crt'))
        self.opener=urllib.request.build_opener(urllib.request.HTTPSHandler(context=context),urllib.request.ProxyHandler({}),workplace.NoRedirect())

    def compose(self,*args):
        command=['docker','compose','--project-name','wms','--file',str(self.config/'compose.yaml'),*args]
        result=subprocess.run(command,capture_output=True,text=True,timeout=180)
        if result.returncode: raise ValueError('Nie udało się zmienić stanu usługi tożsamości')

    def bootstrap(self,client,secret):
        command,environment=bootstrap_command(self.config,client,secret)
        result=subprocess.run(command,env=environment,capture_output=True,text=True,timeout=240)
        if result.returncode: raise ValueError('Oficjalny bootstrap administratora nie powiódł się')

    def sql(self,statement,database='wms'):
        result=subprocess.run(['docker','exec','-i','wms-postgres','psql','-X','-q','-At','-v','ON_ERROR_STOP=1','-U','wms_admin','-d',database],
                              input=statement,text=True,encoding='utf8',capture_output=True,timeout=30)
        if result.returncode: raise ValueError('Nie udało się potwierdzić instalacji')
        return result.stdout.strip()

    def client_exists(self,client):
        if not re.fullmatch(r'wms-workplace-admin-[a-f0-9]{32}',client): raise ValueError('Nieprawidłowy identyfikator konta tymczasowego')
        result=self.sql("SELECT EXISTS(SELECT FROM public.client c JOIN public.realm r ON r.id=c.realm_id WHERE r.name='master' AND c.client_id="+workplace.sql_literal(client)+");",'keycloak')
        if result not in ('t','f'): raise ValueError('Nie udało się potwierdzić konta tymczasowego')
        return result=='t'

    def http(self,method,path,*,body=None,token=None,form=False):
        if not path.startswith('/auth/') or '://' in path or '\\' in path or '..' in path:
            raise ValueError('Nieprawidłowy adres dostawcy tożsamości')
        headers={}
        if token: headers['Authorization']='Bearer '+token
        payload=None
        if body is not None:
            headers['Content-Type']='application/x-www-form-urlencoded' if form else 'application/json'
            payload=(urllib.parse.urlencode(body) if form else json.dumps(body)).encode()
        request=urllib.request.Request(self.origin+path,method=method,headers=headers,data=payload)
        try: response=self.opener.open(request,timeout=30)
        except urllib.error.HTTPError as error: response=error
        except Exception: raise ValueError('Połączenie z dostawcą tożsamości nie powiodło się') from None
        with response:
            raw=response.read(2*1024*1024+1)
            if len(raw)>2*1024*1024: raise ValueError('Odpowiedź przekracza limit')
            try: data=json.loads(raw) if raw else {}
            except ValueError: data={}
            return response.code,data

    def discovery(self):
        deadline=time.monotonic()+240
        path=urllib.parse.urlsplit(self.identity['issuer']).path+'/.well-known/openid-configuration'
        while True:
            try:
                status,data=self.http('GET',path)
                if status==200 and data.get('issuer')==self.identity['issuer']: return
            except ValueError: pass
            if time.monotonic()>=deadline: raise ValueError('Nie udało się potwierdzić uruchomienia dostawcy tożsamości')
            time.sleep(2)

    def admin_token(self,client,secret):
        status,data=self.http('POST','/auth/realms/master/protocol/openid-connect/token',body={'grant_type':'client_credentials','client_id':client,'client_secret':secret},form=True)
        if status!=200 or not isinstance(data.get('access_token'),str): raise ValueError('Uwierzytelnienie administratora nie powiodło się')
        return {key:data[key] for key in ['access_token','refresh_token'] if key in data}

    def cleanup_client(self,state,tokens):
        token=tokens['access_token']; client=state['client_id']
        status,clients=self.http('GET','/auth/admin/realms/master/clients?clientId='+client,token=token)
        if status!=200 or not isinstance(clients,list) or len(clients)!=1 or clients[0].get('clientId')!=client:
            raise ValueError('Nie udało się jednoznacznie potwierdzić konta tymczasowego')
        identifier=str(uuid.UUID(clients[0]['id']))
        revocation_failed=False
        if tokens.get('refresh_token'):
            try:
                status,_=self.http('POST','/auth/realms/master/protocol/openid-connect/revoke',body={'client_id':client,'client_secret':state['secret'],
                    'token':tokens['refresh_token'],'token_type_hint':'refresh_token'},form=True)
                revocation_failed=status!=200
            except Exception: revocation_failed=True
        status,_=self.http('DELETE','/auth/admin/realms/master/clients/'+identifier,token=token)
        if status!=204: raise ValueError('Nie udało się usunąć konta tymczasowego')
        if revocation_failed: raise ValueError('Konto tymczasowe usunięto, lecz wcześniejsze unieważnienie tokena wymaga uzgodnienia')

    def apply(self,token_file,expected):
        return workplace.apply(workplace.Runtime(self.config,token_file),expected,lab=True,credentials_file=self.config/'workplace-users.json')


def main():
    import fcntl
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action',choices=['apply','cleanup'])
    parser.add_argument('--expected-instance-id',required=True)
    parser.add_argument('--lab',action='store_true')
    parser.add_argument('--config',type=Path,default=Path('/etc/wms'))
    args=parser.parse_args()
    if os.name!='posix' or os.geteuid()!=0 or not args.lab: raise ValueError('Wymagane root oraz jawne --lab')
    with Path('/var/lib/wms/operation.lock').open('w') as lock:
        fcntl.flock(lock,fcntl.LOCK_EX|fcntl.LOCK_NB)
        result=provision(Runtime(args.config),args.expected_instance_id,lab=True,state_file=args.config/'workplace-bootstrap-state.json',cleanup_only=args.action=='cleanup')
    print(json.dumps(result,ensure_ascii=False,sort_keys=True))


if __name__=='__main__':
    try: main()
    except Exception:
        print('Konfiguracja stanowiska lub usuwanie konta tymczasowego nie zostały ukończone. Sprawdź prywatny zapis odzyskiwania.',file=sys.stderr)
        raise SystemExit(1)
