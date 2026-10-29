"""The numbered command must remain a required, actively mutated migration."""
from pathlib import Path
import shutil
import tempfile
import unittest

from migrate_core import build_script
import mutation_runner

ROOT=Path(__file__).resolve().parents[1]

class DocumentNumberingMigrationContractTests(unittest.TestCase):
    def test_the_full_campaign_includes_the_independent_sql_numbering_catalog(self):
        load=getattr(mutation_runner,'load_campaign_mutations',None)
        self.assertTrue(callable(load),'The CI campaign must execute numbering faults, not merely unit-test a disconnected catalog')
        cases=load(ROOT)
        numbered=[case for case in cases if case['domain']=='document_numbering']
        self.assertEqual(len(numbered),10)
        self.assertTrue(all(case['kind']=='sql_function' and case['pattern'].startswith('^TestNumberingCritic') for case in numbered))

    def test_required_api_numbering_faults_are_present_in_the_campaign(self):
        names={case['name'] for case in mutation_runner.load_core_mutations(ROOT)}
        self.assertTrue({'core_boundary_document_auto_number_required_again','core_boundary_document_auto_number_for_wrong_kind',
                         'core_boundary_document_whitespace_number_accepted'} <= names,
                        'Automatic numbering must retain HTTP validation mutants in the full campaign')

    def test_numbering_migration_cannot_be_omitted_from_an_update_package(self):
        with tempfile.TemporaryDirectory() as temporary:
            root=Path(temporary)
            shutil.copytree(ROOT/'db/migrations',root/'db/migrations')
            (root/'db/migrations/0010_document_numbering.sql').unlink(missing_ok=True)
            with self.assertRaisesRegex(ValueError,'Missing published migration.*0010_document_numbering'):
                build_script(root)

    def test_mutations_follow_the_latest_document_command_definition(self):
        extract=getattr(mutation_runner,'document_function_source',None)
        self.assertTrue(callable(extract),'Mutation gate must select the current command rather than its superseded migration')
        with tempfile.TemporaryDirectory() as temporary:
            root=Path(temporary);directory=root/'db/migrations';directory.mkdir(parents=True)
            (directory/'0009_old.sql').write_text('CREATE FUNCTION warehouse.command_document() RETURNS void LANGUAGE plpgsql AS $$ BEGIN old_rule; END $$;')
            (directory/'0010_numbered.sql').write_text('CREATE OR REPLACE FUNCTION warehouse.command_document() RETURNS void LANGUAGE plpgsql AS $$ BEGIN current_rule; END $$;')
            name,sql=extract(root)
            self.assertEqual(name,'db/migrations/0010_numbered.sql')
            self.assertIn('current_rule',sql)
            self.assertNotIn('old_rule',sql)
            self.assertEqual(sql.count('CREATE OR REPLACE FUNCTION'),1)

    def test_an_unavailable_document_command_is_an_invalid_campaign(self):
        extract=getattr(mutation_runner,'document_function_source',None)
        self.assertTrue(callable(extract),'Missing SQL definition must fail closed')
        with tempfile.TemporaryDirectory() as temporary:
            with self.assertRaisesRegex(ValueError,'document command'):
                extract(Path(temporary))

if __name__=='__main__':unittest.main()
