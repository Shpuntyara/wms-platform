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
		{name: "hardened", profile: "hardened-v1", key: key, worker: "postgres://worker@127.0.0.1/db", valid: true},
		{name: "unknown profile", profile: "hardened-v2", key: key, worker: "postgres://worker@127.0.0.1/db"},
		{name: "missing signing key", profile: "hardened-v1", worker: "postgres://worker@127.0.0.1/db"},
		{name: "missing worker login", profile: "hardened-v1", key: key},
		{name: "bad base64", profile: "hardened-v1", key: "invalid secret value", worker: "postgres://worker@127.0.0.1/db"},
		{name: "short key", profile: "hardened-v1", key: base64.StdEncoding.EncodeToString(make([]byte, 16)), worker: "postgres://worker@127.0.0.1/db"},
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
		name, apiLogin, workerLogin, apiOrg, apiWarehouse, workerOrg, workerWarehouse string
		valid                                                                         bool
	}{
		{name: "same warehouse distinct logins", apiLogin: "api", workerLogin: "worker", apiOrg: "org", apiWarehouse: "warehouse", workerOrg: "org", workerWarehouse: "warehouse", valid: true},
		{name: "same effective login", apiLogin: "api", workerLogin: "api", apiOrg: "org", apiWarehouse: "warehouse", workerOrg: "org", workerWarehouse: "warehouse"},
		{name: "foreign organization", apiLogin: "api", workerLogin: "worker", apiOrg: "org", apiWarehouse: "warehouse", workerOrg: "other", workerWarehouse: "warehouse"},
		{name: "foreign warehouse", apiLogin: "api", workerLogin: "worker", apiOrg: "org", apiWarehouse: "warehouse", workerOrg: "org", workerWarehouse: "other"},
		{name: "empty scope", apiLogin: "api", workerLogin: "worker"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateHardenedBinding(tc.apiLogin, tc.workerLogin, tc.apiOrg, tc.apiWarehouse, tc.workerOrg, tc.workerWarehouse); (err == nil) != tc.valid {
				t.Fatalf("accepted=%v want=%v", err == nil, tc.valid)
			}
		})
	}
}
