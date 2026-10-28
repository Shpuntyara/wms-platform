package main

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestDatabaseProfileConfigurationFailsClosed(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	for _, tc := range []struct {
		name, profile, key, worker string
		valid                      bool
	}{
		{name: "default requires hardened configuration"},
		{name: "hardened default", key: key, worker: "postgres://worker@127.0.0.1/db", valid: true},
		{name: "explicit legacy", profile: "legacy", valid: true},
		{name: "on premise", profile: "onprem-v1", key: key, worker: "postgres://worker@127.0.0.1/db", valid: true},
		{name: "old single warehouse profile requires upgrade", profile: "hardened-v1", key: key, worker: "postgres://worker@127.0.0.1/db"},
		{name: "unknown profile", profile: "hardened-v2", key: key, worker: "postgres://worker@127.0.0.1/db"},
		{name: "missing signing key", profile: "onprem-v1", worker: "postgres://worker@127.0.0.1/db"},
		{name: "missing worker login", profile: "onprem-v1", key: key},
		{name: "bad base64", profile: "onprem-v1", key: "invalid secret value", worker: "postgres://worker@127.0.0.1/db"},
		{name: "short key", profile: "onprem-v1", key: base64.StdEncoding.EncodeToString(make([]byte, 16)), worker: "postgres://worker@127.0.0.1/db"},
		{name: "hardened settings accidentally in legacy", key: key},
		{name: "worker setting accidentally in legacy", worker: "postgres://worker@127.0.0.1/db"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profile, err := parseDatabaseProfile(tc.profile, tc.key, tc.worker)
			if (err == nil) != tc.valid {
				t.Fatalf("accepted=%v want=%v", err == nil, tc.valid)
			}
			if err != nil && strings.Contains(err.Error(), tc.key) && tc.key != "" {
				t.Fatal("configuration error disclosed the signing key")
			}
			if err == nil && profile.hardened && (len(profile.contextKey) != 32 || profile.workerURL == "") {
				t.Fatal("incomplete hardened configuration")
			}
		})
	}
}

func TestHardenedWorkerUsesSeparateLoginOnTheSameDatabase(t *testing.T) {
	api, err := pgxpool.ParseConfig("host=127.0.0.1 port=55432 dbname=tenant user=api sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, dsn string
		valid     bool
	}{
		{name: "separate login", dsn: "host=127.0.0.1 port=55432 dbname=tenant user=worker sslmode=disable", valid: true},
		{name: "same login", dsn: "host=127.0.0.1 port=55432 dbname=tenant user=api sslmode=disable"},
		{name: "wrong database", dsn: "host=127.0.0.1 port=55432 dbname=another user=worker sslmode=disable"},
		{name: "wrong port", dsn: "host=127.0.0.1 port=55433 dbname=tenant user=worker sslmode=disable"},
		{name: "wrong host", dsn: "host=localhost port=55432 dbname=tenant user=worker sslmode=disable"},
		{name: "worker fallback targets another endpoint", dsn: "host=127.0.0.1,localhost port=55432 dbname=tenant user=worker sslmode=disable"},
		{name: "TLS fallback on same endpoint", dsn: "host=127.0.0.1 port=55432 dbname=tenant user=worker sslmode=prefer", valid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			worker, err := pgxpool.ParseConfig(tc.dsn)
			if err != nil {
				t.Fatal(err)
			}
			if err := validateHardenedWorkerTarget(api.ConnConfig, worker.ConnConfig); (err == nil) != tc.valid {
				t.Fatalf("accepted=%v want=%v", err == nil, tc.valid)
			}
		})
	}
}

func TestHardenedAPIRejectsFallbackToAnotherEndpoint(t *testing.T) {
	api, err := pgxpool.ParseConfig("host=127.0.0.1,localhost port=55432 dbname=tenant user=api sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	worker, err := pgxpool.ParseConfig("host=127.0.0.1 port=55432 dbname=tenant user=worker sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	if err := validateHardenedWorkerTarget(api.ConnConfig, worker.ConnConfig); err == nil {
		t.Fatal("API fallback can redirect the pool to another database endpoint")
	}
}

func TestHardenedDeploymentRejectsMismatchedBinding(t *testing.T) {
	for _, tc := range []struct {
		name, apiLogin, workerLogin, apiInstallation, workerInstallation string
		valid                                                            bool
	}{
		{name: "same client installation distinct logins", apiLogin: "api", workerLogin: "worker", apiInstallation: "10000000-0000-4000-8000-000000000001", workerInstallation: "10000000-0000-4000-8000-000000000001", valid: true},
		{name: "same effective login", apiLogin: "api", workerLogin: "api", apiInstallation: "10000000-0000-4000-8000-000000000001", workerInstallation: "10000000-0000-4000-8000-000000000001"},
		{name: "different client installation", apiLogin: "api", workerLogin: "worker", apiInstallation: "10000000-0000-4000-8000-000000000001", workerInstallation: "20000000-0000-4000-8000-000000000001"},
		{name: "malformed installation", apiLogin: "api", workerLogin: "worker", apiInstallation: "client", workerInstallation: "client"},
		{name: "nil installation", apiLogin: "api", workerLogin: "worker", apiInstallation: "00000000-0000-0000-0000-000000000000", workerInstallation: "00000000-0000-0000-0000-000000000000"},
		{name: "empty installation", apiLogin: "api", workerLogin: "worker"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateOnPremBinding(tc.apiLogin, tc.workerLogin, tc.apiInstallation, tc.workerInstallation); (err == nil) != tc.valid {
				t.Fatalf("accepted=%v want=%v", err == nil, tc.valid)
			}
		})
	}
}
