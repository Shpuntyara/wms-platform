"""Address recovery must retain installation identity and existing actor IDs."""
import argparse
import contextlib
import hashlib
import importlib.util
import inspect
import io
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location('address_installer', ROOT / 'deploy/linux/wmsctl.py')
wms = importlib.util.module_from_spec(spec)
spec.loader.exec_module(wms)
OPENSSL = shutil.which('openssl') or r'C:\Program Files\Git\usr\bin\openssl.exe'
OLD, NEW = '192.0.2.10', '192.0.2.20'
INSTANCE = '9115926d-9f22-44ab-840c-afcb3a2a2ac3'
OLD_ISSUER = 'https://' + OLD + '/auth/realms/wms-' + INSTANCE[:8]
NEW_ISSUER = 'https://' + NEW + '/auth/realms/wms-' + INSTANCE[:8]


def openssl(*argv):
    result = subprocess.run([OPENSSL, *map(str, argv)], capture_output=True, text=True, timeout=30)
    if result.returncode:
        raise RuntimeError('Test certificate command failed')
    return result.stdout.strip()


def certificate(directory, address):
    directory.mkdir(parents=True)
    openssl('req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-days', '2', '-subj', '/CN=Test private CA',
            '-keyout', directory / 'ca.key', '-out', directory / 'ca.crt')
    openssl('req', '-newkey', 'rsa:2048', '-nodes', '-subj', '/CN=' + address,
            '-keyout', directory / 'server.key', '-out', directory / 'server.csr')
    (directory / 'extensions').write_text('subjectAltName=IP:' + address + '\nextendedKeyUsage=serverAuth\n')
    openssl('x509', '-req', '-in', directory / 'server.csr', '-CA', directory / 'ca.crt',
            '-CAkey', directory / 'ca.key', '-CAcreateserial', '-days', '1', '-extfile', directory / 'extensions',
            '-out', directory / 'server.crt')


class AddressRecoveryTests(unittest.TestCase):
    def tls(self, base, address):
        def command(argv, *args, **kwargs):
            if str(argv[0]) == 'openssl':
                return openssl(*argv[1:])
            return ''
        with patch.object(wms, 'CONFIG', base), patch.object(wms, 'run', side_effect=command), \
             patch.object(wms.shutil, 'copyfile'):
            wms.tls(address)

    def test_actual_leaf_certificate_changes_ip_without_rotating_keys_or_ca(self):
        with tempfile.TemporaryDirectory() as tmp:
            base = Path(tmp)
            certificate(base / 'tls', OLD)
            before = {name: (base / 'tls' / name).read_bytes() for name in ('ca.key', 'ca.crt', 'server.key')}
            self.tls(base, NEW)
            checked = subprocess.run([OPENSSL, 'verify', '-CAfile', str(base / 'tls/ca.crt'),
                '-verify_ip', NEW, '-purpose', 'sslserver', str(base / 'tls/server.crt')], capture_output=True)
            self.assertEqual(checked.returncode, 0, 'Installed certificate still identifies the previous DHCP address')
            for name, expected in before.items():
                self.assertEqual(hashlib.sha256((base / 'tls' / name).read_bytes()).hexdigest(),
                                 hashlib.sha256(expected).hexdigest(), name + ' was rotated')

    def test_valid_leaf_does_not_change_and_missing_private_key_is_not_regenerated(self):
        with tempfile.TemporaryDirectory() as tmp:
            base = Path(tmp)
            certificate(base / 'tls', NEW)
            original = (base / 'tls/server.crt').read_bytes()
            self.tls(base, NEW)
            self.assertEqual((base / 'tls/server.crt').read_bytes(), original)
            (base / 'tls/server.key').unlink()
            try:
                self.tls(base, NEW)
            except Exception as error:
                self.assertIsInstance(error, ValueError, 'Missing identity key needs a fail-closed diagnosis')
            else:
                self.fail('Existing identity certificate without its key must not be accepted')

    def test_backup_can_leave_keycloak_stopped_until_its_new_origin_is_ready(self):
        self.assertIn('resume', inspect.signature(wms.backup).parameters,
                      'An address cutover cannot resume and poll the unreachable previous origin')
        with tempfile.TemporaryDirectory() as tmp:
            base = Path(tmp)
            config = base / 'config'
            config.mkdir()
            (config / 'identity.json').write_text('{}')
            def dump(database, target):
                target.write_bytes(database.encode())
            fingerprint = {'test.table': {'rows': 1, 'sha256': 'a' * 64}}
            with patch.object(wms, 'STATE', base / 'state'), patch.object(wms, 'CONFIG', config), \
                 patch.object(wms, 'run', return_value='true'), \
                 patch.object(wms, 'database_fingerprints', return_value=fingerprint), \
                 patch.object(wms, 'keycloak_identity', return_value={'identity': True}), \
                 patch.object(wms, 'dump_database', side_effect=dump), \
                 patch.object(wms, 'restore_database', return_value={'restore_verified': True}) as restore, \
                 patch.object(wms, 'resume_keycloak', side_effect=AssertionError('old origin polled')):
                proof = wms.backup({'instance_id': INSTANCE}, resume=False)
            self.assertTrue(proof['restore_verified'])
            self.assertEqual(restore.call_count, 2)

    def test_backup_includes_only_the_bound_separate_workspace_file(self):
        with tempfile.TemporaryDirectory() as tmp:
            base = Path(tmp)
            config = base / 'config'; config.mkdir()
            (config / 'identity.json').write_text('{}')
            workspace = base / 'wms-workplace/workspace.json'
            workspace.parent.mkdir()
            workspace.write_text('{"organizations":[]}')
            (workspace.parent / 'unrelated-private-file').write_text('not part of this backup')
            def dump(database, target): target.write_bytes(database.encode())
            fingerprint = {'test.table': {'rows':1,'sha256':'a'*64}}
            with patch.object(wms,'CONFIG',config), patch.object(wms,'STATE',base/'state'), \
                 patch.object(wms,'WORKSPACE_CONFIG',workspace), patch.object(wms,'run',return_value='false'), \
                 patch.object(wms,'database_fingerprints',return_value=fingerprint), \
                 patch.object(wms,'keycloak_identity',return_value={'identity':True}), \
                 patch.object(wms,'dump_database',side_effect=dump), \
                 patch.object(wms,'restore_database',return_value={'restore_verified':True}):
                receipt=wms.backup({'instance_id':INSTANCE})
            import tarfile
            with tarfile.open(receipt['artifacts']['identity.tar.gz']['path']) as archive:
                self.assertIn('wms-workplace/workspace.json',archive.getnames())
                self.assertNotIn('wms-workplace/unrelated-private-file',archive.getnames())

    def test_explicit_address_update_uses_verified_backup_and_waits_for_the_new_issuer(self):
        with tempfile.TemporaryDirectory() as tmp:
            base = Path(tmp)
            config, state = base / 'config', base / 'state'
            config.mkdir(); state.mkdir(); (base / 'current').mkdir(); (base / 'release').mkdir()
            identity = {'instance_id': INSTANCE, 'address': OLD, 'issuer': OLD_ISSUER}
            (config / 'identity.json').write_text(json.dumps(identity))
            args = argparse.Namespace(action='update', address=NEW, previous_address=OLD,
                archive=base / 'release.tar', sha256='a' * 64, revision='b' * 40)
            order, rows = [], [{'id':'00000000-0000-4000-8000-000000000002','subject':'stable-service-subject','issuer':OLD_ISSUER}]
            settings = dict(wms.DURABILITY, synchronous_commit='on')
            def command(argv, *args, **kwargs):
                order.append(list(map(str, argv)))
                return 'active' if argv[:2] == ['systemctl', 'is-active'] else ('[]' if argv[:3] == ['docker','image','inspect'] else '')
            def configure(value, address):
                order.append('configuration')
                (config / 'identity.json').write_text(json.dumps(dict(identity,address=address,issuer=NEW_ISSUER)))
                return NEW_ISSUER
            def query(text, database='wms'):
                if text.startswith('BEGIN;'):
                    order.append('actor-rebind')
                    for row in rows:
                        row['issuer'] = NEW_ISSUER
                    return ''
                if 'json_agg' in text or 'jsonb_agg' in text:
                    return json.dumps(rows)
                return '1'
            def discovery(url):
                order.append('new-discovery')
                self.assertTrue(url.startswith(NEW_ISSUER))
                return {'issuer':NEW_ISSUER}
            original_replace = os.replace
            def replace(source, target):
                if Path(target) == base / 'current':
                    order.append('release-switch')
                    return
                return original_replace(source, target)
            with contextlib.ExitStack() as stack:
                stack.enter_context(patch.object(Path,'symlink_to'))
                stack.enter_context(patch.object(wms.os,'replace',side_effect=replace))
                patches = {'CONFIG':config,'STATE':state,'BASE':base,'run':command,
                    'release_extract':lambda *a:base/'release','configuration':configure,
                    'fetch_images':lambda:None,'compose':lambda *a:None,'sql':query,
                    'database_settings':lambda:settings,'validate_update_preflight':lambda *a:None,
                    'database_bootstrap':lambda *a:None,'tls':lambda *a:None,'units':lambda *a:None,
                    'wait_for':lambda f,*a,**k:f(),'http_json':discovery,'health_json':lambda *a:True}
                for name, replacement in patches.items():
                    stack.enter_context(patch.object(wms,name,replacement))
                backup = stack.enter_context(patch.object(wms,'backup',return_value={'restore_verified':True}))
                stack.enter_context(contextlib.redirect_stdout(io.StringIO()))
                try:
                    wms.install(args,{'instance_id':INSTANCE})
                except ValueError as error:
                    self.fail('Explicit previous-address recovery was refused: ' + str(error))
            backup.assert_called_once_with({'instance_id':INSTANCE},resume=False)
            self.assertLess(order.index('new-discovery'), order.index('actor-rebind'))
            self.assertEqual(rows[0]['issuer'],NEW_ISSUER)
            self.assertEqual(rows[0]['subject'],'stable-service-subject')

    def test_unapproved_or_mismatched_previous_address_still_refuses_before_mutation(self):
        with tempfile.TemporaryDirectory() as tmp:
            base=Path(tmp)
            (base/'identity.json').write_text(json.dumps({'instance_id':INSTANCE,'address':OLD,'issuer':OLD_ISSUER}))
            for previous in (None,'192.0.2.99'):
                args=argparse.Namespace(action='update',address=NEW,previous_address=previous,
                    archive=base/'release.tar',sha256='a'*64,revision='b'*40)
                with patch.object(wms,'CONFIG',base), patch.object(wms,'STATE',base/'state'), \
                     patch.object(wms,'BASE',base/'installation'), patch.object(wms,'release_extract') as extract:
                    with self.assertRaises(ValueError):
                        wms.install(args,{'instance_id':INSTANCE})
                    extract.assert_not_called()

    def test_recorded_actor_snapshot_cannot_adopt_changed_subjects_or_extra_actors(self):
        change={'old_issuer':OLD_ISSUER,'new_issuer':NEW_ISSUER,'actor_bindings':[
            {'id':'00000000-0000-4000-8000-000000000002','subject':'original'}]}
        foreign=[{'id':'00000000-0000-4000-8000-000000000003','subject':'foreign','issuer':NEW_ISSUER}]
        with patch.object(wms,'sql',return_value=json.dumps(foreign)), patch.object(wms,'private_write') as write:
            with self.assertRaises(ValueError):
                wms._prepare_address_change(change,{'restore_verified':True})
            write.assert_not_called()

    def test_preexisting_target_issuer_cannot_be_merged_into_this_installation(self):
        change={'old_issuer':OLD_ISSUER,'new_issuer':NEW_ISSUER}
        foreign=[{'id':'00000000-0000-4000-8000-000000000003','subject':'foreign','issuer':NEW_ISSUER}]
        with patch.object(wms,'sql',return_value=json.dumps(foreign)), patch.object(wms,'private_write') as write:
            with self.assertRaises(ValueError):
                wms._prepare_address_change(change,{'restore_verified':True})
            write.assert_not_called()

    def test_configured_workplace_address_change_refuses_before_any_mutation(self):
        for public,private in [(True,False),(False,True),(True,True)]:
            with self.subTest(public=public,private=private),tempfile.TemporaryDirectory() as tmp:
                base=Path(tmp); config=base/'config'; config.mkdir()
                workspace=base/'workspace/workspace.json'; workspace.parent.mkdir()
                (config/'identity.json').write_text(json.dumps({'instance_id':INSTANCE,'address':OLD,'issuer':OLD_ISSUER}))
                if public: workspace.write_text(json.dumps({'oidc':{'url':'https://'+OLD+'/auth'}}))
                if private: (config/'workplace-users.json').write_text(json.dumps({'instance_id':INSTANCE,'origin':'https://'+OLD,'users':{}}))
                before={p.relative_to(base).as_posix():p.read_bytes() for p in base.rglob('*') if p.is_file()}
                args=argparse.Namespace(action='update',address=NEW,previous_address=OLD,
                    archive=base/'release.tar',sha256='a'*64,revision='b'*40)
                caught=None
                with patch.object(wms,'CONFIG',config),patch.object(wms,'STATE',base/'state'), \
                     patch.object(wms,'WORKSPACE_CONFIG',workspace),patch.object(wms,'BASE',base/'installation'), \
                     patch.object(wms,'release_extract',side_effect=RuntimeError('test release stage')) as extract, \
                     patch.object(wms,'run') as run,patch.object(wms,'configuration') as configuration, \
                     patch.object(wms,'backup') as backup,patch.object(wms,'sql') as sql:
                    try: wms.install(args,{'instance_id':INSTANCE})
                    except Exception as error: caught=error
                self.assertIsInstance(caught,ValueError,'Configured workplace address change reached release extraction')
                self.assertIn('Zmiana adresu',str(caught))
                extract.assert_not_called();run.assert_not_called();configuration.assert_not_called();backup.assert_not_called();sql.assert_not_called()
                after={p.relative_to(base).as_posix():p.read_bytes() for p in base.rglob('*') if p.is_file()}
                self.assertEqual(after,before,'Fail-closed address guard changed installed identity or employee metadata')

    def test_same_address_workplace_update_still_reaches_the_release_stage(self):
        with tempfile.TemporaryDirectory() as tmp:
            base=Path(tmp); config=base/'config'; config.mkdir()
            workspace=base/'workspace.json';workspace.write_text('{"oidc":{}}')
            (config/'identity.json').write_text(json.dumps({'instance_id':INSTANCE,'address':OLD,'issuer':OLD_ISSUER}))
            args=argparse.Namespace(action='update',address=OLD,previous_address=None,
                archive=base/'release.tar',sha256='a'*64,revision='b'*40)
            with patch.object(wms,'CONFIG',config),patch.object(wms,'STATE',base/'state'), \
                 patch.object(wms,'WORKSPACE_CONFIG',workspace),patch.object(wms,'release_extract',side_effect=RuntimeError('test release stage')) as extract:
                with self.assertRaisesRegex(RuntimeError,'test release stage'):
                    wms.install(args,{'instance_id':INSTANCE})
            extract.assert_called_once()

    def test_configured_workplace_cannot_resume_a_pending_origin_rebind(self):
        with tempfile.TemporaryDirectory() as tmp:
            base=Path(tmp);config=base/'config';state=base/'state';config.mkdir();state.mkdir()
            (config/'identity.json').write_text(json.dumps({'instance_id':INSTANCE,'address':NEW,'issuer':NEW_ISSUER}))
            (state/'address-change.json').write_text(json.dumps({'instance_id':INSTANCE,'old_address':OLD,'new_address':NEW,'old_issuer':OLD_ISSUER,'new_issuer':NEW_ISSUER,'status':'prepared'}))
            workspace=base/'workspace.json';workspace.write_text(json.dumps({'oidc':{'url':'https://'+OLD+'/auth'}}))
            args=argparse.Namespace(action='update',address=NEW,previous_address=OLD)
            with patch.object(wms,'CONFIG',config),patch.object(wms,'STATE',state),patch.object(wms,'WORKSPACE_CONFIG',workspace):
                with self.assertRaisesRegex(ValueError,'Zmiana adresu'):
                    wms._address_change(args,{'instance_id':INSTANCE})


if __name__ == '__main__':
    unittest.main()
