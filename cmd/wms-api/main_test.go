package main

import (
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestDatabaseTLSChecksEveryEndpoint(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, dsn string
		valid     bool
	}{
		{name: "local IPv4", dsn: "host=127.0.0.1 sslmode=disable", valid: true},
		{name: "local IPv6", dsn: "host=::1 sslmode=disable", valid: true},
		{name: "verified remote", dsn: "host=db.example.invalid sslmode=verify-full", valid: true},
		{name: "remote plaintext", dsn: "host=db.example.invalid sslmode=disable"},
		{name: "unverified remote TLS", dsn: "host=db.example.invalid sslmode=require"},
		{name: "plaintext fallback", dsn: "host=db.example.invalid sslmode=prefer"},
		{name: "local primary remote fallback", dsn: "host=localhost,db.example.invalid sslmode=disable"},
		{name: "verified multiple hosts", dsn: "host=db1.example.invalid,db2.example.invalid sslmode=verify-full", valid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			config, err := pgconn.ParseConfig(tc.dsn)
			if err != nil {
				t.Fatal(err)
			}
			if err := validateDatabaseTLS(config); (err == nil) != tc.valid {
				t.Fatalf("error=%v, want valid=%v", err, tc.valid)
			}
		})
	}
}

func TestMonitoringListenerMustStayOnLoopback(t *testing.T) {
	for _, address := range []string{"0.0.0.0:9091", "[::]:9091", "10.0.0.1:9091", "example.com:9091", "localhost:9091", "invalid"} {
		if validateMonitorAddress(address) == nil {
			t.Fatalf("unsafe monitoring bind accepted: %s", address)
		}
	}
	for _, address := range []string{"127.0.0.1:9091", "[::1]:9091"} {
		if err := validateMonitorAddress(address); err != nil {
			t.Fatal(err)
		}
	}
}
