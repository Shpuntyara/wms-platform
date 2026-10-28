"""A green report must prove the newly required document regressions ran."""
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parent.parent
DOCUMENT_REGRESSIONS = (
    'TestDocumentCommandsRecoverFromCommitTransportFailure',
    'TestDocumentCommandsPreserveDefinitiveCommitRejection',
    'TestDocumentCommandProperties',
    'TestDocumentInvalidGeneratedLineRollsBackWholeCreate',
    'TestDocumentReadsStayBoundedAsHistoryGrows',
    'TestDocumentPaginationProperties',
    'TestDocumentAdversarialCommandIdentityAndAuthorization',
    'TestDocumentAdversarialConcurrentCommands',
    'TestDocumentAdversarialOwnerFanoutHydratesOnlyPage',
    'TestDocumentAdversarialCursorCannotCrossAuthorizationScope',
    'TestIdentifierNormalizationProperties',
    'TestKernelAuthorizationDeclarativeAlternativesRequireAllConjuncts',
    'TestKernelAuthorizationDeclarativeFactsRemainScopedAndUnique',
    'TestKernelAuthorizationTypedRulesNeverCoerceOrRound',
    'TestKernelBackgroundRejectsInvalidBatchSizes',
    'TestKernelExpirySkipsLockedDocumentAndRollsBackAtomically',
    'TestKernelInventoryBalanceIdentityIsConcurrentAndNullSafe',
    'TestKernelInventoryBusinessRejectionsAreAtomic',
    'TestKernelInventoryCommandReplayBindsExactRequest',
    'TestKernelInventoryCompetingDocumentsCannotOverdrawSharedStock',
    'TestKernelInventoryCorrectionCannotReferenceDifferentScope',
    'TestKernelInventoryDirectCommandsRejectStaleVersionAndWrongKind',
    'TestKernelInventoryExpiredReservationCannotShipButOriginalCommandReplays',
    'TestKernelInventoryGeneratedWorkflowConservesLedger',
    'TestKernelInventoryHistoryAndBalanceIdentityAreImmutable',
    'TestKernelInventoryItemAccountingCannotDriftAfterUse',
    'TestKernelInventoryPrepareCannotCommitIncompleteOperation',
    'TestKernelInventoryReleaseAndExpiryKeepExactEffects',
    'TestKernelInventoryTrackingMismatchCannotRelabelStock',
    'TestKernelOutboxConcurrentClaimsAndLeaseRecovery',
    'TestKernelOutboxRetryAvailabilityAndDeadLetter',
    'TestOIDCAdversarialAlgorithmsAndLifetime',
    'TestPrincipalAdversarialGrantScopesAreExact',
    'TestQuantityCanonicalDecimalProperties',
    'TestRegistryAdversarialManifestSnapshotIsImmutable',
    'TestRegistryAdversarialRawPolicySnapshotIsImmutable',
    'TestRequestBodyLimitIncludesTrailingJSONAndWhitespace',
    'TestRequestDeadlineIsShorterThanTheCallerContext',
    'TestSenderCopiesSigningKeyAndChecksResponseStatus',
    'TestSenderInvalidEnvelopeNeverMakesRequest',
    'TestWorkerAckFailureDoesNotCountAsDelivery',
    'TestWorkerCancellationReleasesEveryActiveSender',
    'TestWorkerFreshHealthExpiresDuringMonitoringFailure',
    'TestWorkerRejectsPartialFailedClaim',
    'TestQueryFiltersCannotDisappearOnMalformedEncoding',
    'TestRequestBodySizeBoundaryIsExact',
)


class VerificationGateTests(unittest.TestCase):
    def test_omitting_a_new_regression_fails_the_complete_report(self):
        existing = json.loads((ROOT / 'docs/verification/warehouse-green.json').read_text())['critical_regressions']
        names = set(existing) | set(DOCUMENT_REGRESSIONS) | {'TestDispatchQualityModuleUsesTransactionalFacts'}
        for omitted in DOCUMENT_REGRESSIONS:
            with self.subTest(omitted=omitted), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                events = [{'Action': 'pass', 'Test': name, 'Package': 'postgres'} for name in names if name != omitted]
                (root / 'events.jsonl').write_text('\n'.join(json.dumps(e) for e in events))
                (root / 'legacy.json').write_text(json.dumps({'status': 'passed', 'passed': 41, 'failed': 0}))
                result = subprocess.run([sys.executable, str(ROOT / 'scripts/summarize-tests.py'), '--events', str(root / 'events.jsonl'), '--legacy', str(root / 'legacy.json'), '--output', str(root / 'report.json'), '--go', 'test', '--postgres', 'test'], capture_output=True, text=True)
                self.assertNotEqual(result.returncode, 0, 'Missing regression was reported green: ' + omitted)
                self.assertEqual(json.loads((root / 'report.json').read_text())['status'], 'failed')


if __name__ == '__main__':
    unittest.main()
