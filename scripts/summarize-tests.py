"""Summarize real go test JSON events without storing connection credentials."""
import argparse
import json
from pathlib import Path

parser = argparse.ArgumentParser()
parser.add_argument('--events', required=True)
parser.add_argument('--legacy', required=True)
parser.add_argument('--output', required=True)
parser.add_argument('--go', required=True)
parser.add_argument('--postgres', required=True)
args = parser.parse_args()
events = [json.loads(line) for line in Path(args.events).read_text(encoding='utf-8-sig').splitlines() if line.strip()]
tests = [e for e in events if e.get('Test') and e.get('Action') in ('pass', 'fail', 'skip')]
failed = [e for e in tests if e['Action'] == 'fail']
skipped = [e for e in tests if e['Action'] == 'skip']
packages = {e['Package']: e['Action'] for e in events if not e.get('Test') and e.get('Action') in ('pass', 'fail', 'skip')}
legacy = json.loads(Path(args.legacy).read_text(encoding='utf-8'))
integration_ran = any(e.get('Test') == 'TestDispatchQualityModuleUsesTransactionalFacts' and e.get('Action') == 'pass' for e in tests)
required = [
    'TestQueryFiltersCannotDisappearOnMalformedEncoding',
    'TestRequestBodySizeBoundaryIsExact',
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
    'TestRelayDrainsBurstWithoutPerEventPollingDelay',
    'TestSlowDeliveryDoesNotHoldOtherSlots',
    'TestBackgroundReadinessDetectsStallsAndBacklogs',
    'TestBackgroundMonitorExposesCountersAndUnhealthyStatus',
    'TestReadinessSurvivesBackgroundFailure',
    'TestBrokerOutageKeepsAPIAvailableAndRecovers',
    'TestWarehouseReceiptPutawayPickAndShipViaAPI',
    'TestDocumentCommandsRejectScopeAndInvalidLines',
    'TestDocumentRetryIsAtomicAndSQLChecksPermissions',
    'TestNewWarehouseDocumentsRetainMandatorySQLGuards',
    'TestDocumentInputValidationPrecedesBackend',
    'TestMonitoringListenerMustStayOnLoopback',
    'TestSQLGuardIsOnlyBusinessDecision',
    'TestUninstalledHostGuardsCannotAuthorizeCommands',
    'TestDeclarativeV2ValidatesAlternativesAndTypedRules',
    'TestDeclarativeV2SQLAlternativesAndExactDecimals',
    'TestRelayDatabaseHTTPBurst',
    'TestBackgroundQueueHealthReportsDeadAndExpired',
    'TestSecurityRuntimeRejectsServerFileAndDatabaseOwnershipPrivileges',
    'TestSecurityRuntimeCannotOwnModuleExecutionSurface',
    'TestSecurityDeclarativePolicyEnforcedByAPIAndSQL',
    'TestSecurityDispatchUpgradeTo12',
    'TestSecurityDirectSQLCannotBypassDispatchGuard',
    'TestSecurityDisabledModuleStillBlocksSQL',
    'TestSecurityRegistryCannotOmitInstalledModule',
    'TestSecurityExpiryReleasesExactlyOnce',
    'TestSecurityQualityRequiresAuthorizedIndependentReviewerAndCurrentPolicy',
    'TestSecurityOutboxLeaseFencesStaleAcknowledgement',
    'TestDeclarativeModuleDoesNotNeedCompiledBuild',
    'TestSenderSignsExactEventAndRejectsRedirect',
    'TestSenderConfigurationAndNetworkBoundary',
    'TestDeliveryFailureSchedulesRetry',
    'TestOIDCSecureRedirects',
    'TestRegistryRejectsRelocatedGuard',
    'TestStoreRejectsDirectCoreWrites',
    'TestLostCommitAcknowledgementRecoversByCommandID',
    'TestModuleInstallationIntegrity',
    'TestMovementAPIReceiptTransferAndCorrection',
    'TestPickingRelocatesReservationBeforeShipment',
    'TestWritesRejectCrossOwnerWarehouseAndOrganization',
    'TestDispatchQualityUpgradePreservesDataAndHistory',
    'TestDispatchQualityUpgradeRejectsUnexpectedInstallation',
]
observed = {e['Test'] for e in tests if e['Action'] == 'pass'}
critical = {name: name in observed for name in required}
passed = not failed and not skipped and 'fail' not in packages.values() and integration_ran and all(critical.values()) and legacy['status'] == 'passed'
report = {
    'status': 'passed' if passed else 'failed',
    'tdd_test_commit': '8032682',
    'tdd_evidence': 'docs/verification/security-red.json',
    'additional_tdd_evidence': 'docs/verification/background-red.json',
    'current_change_tdd_evidence': [
        'docs/verification/core-audit/api-tdd.json',
        'docs/verification/core-audit/authorization.json',
        'docs/verification/core-audit/background.json',
        'docs/verification/migration-checksum-tdd.json',
        'docs/verification/document-commands-tdd.json',
        'docs/verification/document-reads/tdd.json',
    ],
    'current_change_test_commit': '8a9d675',
    'current_change_test_commits': ['8a9d675', 'c4f26c0'],
    'go': args.go.strip(),
    'postgresql': args.postgres.strip(),
    'command': 'go test -race -tags=integration ./... -count=1 -timeout=600s -json',
    'race_detector': True,
    'integration_tests_ran': integration_ran,
    'critical_regressions': critical,
    'passed_test_events': sum(e['Action'] == 'pass' for e in tests),
    'failed_test_events': len(failed),
    'skipped_test_events': len(skipped),
    'packages': packages,
    'sql_kernel_regressions': {'passed': legacy['passed'], 'failed': legacy['failed']},
    'scope': 'Local isolated PostgreSQL, OIDC cryptography, HTTP contract, permissions, guards, concurrency and SQL regressions. No production load, HA, external IdP deployment or power-loss verification.',
}
target = Path(args.output)
target.parent.mkdir(parents=True, exist_ok=True)
target.write_text(json.dumps(report, ensure_ascii=False, indent=2) + '\n', encoding='utf-8')
print(json.dumps(report, ensure_ascii=False, indent=2))
raise SystemExit(0 if passed else 1)
