//go:build integration

package postgres_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Shpuntyara/wms-platform/internal/clientmodules/dispatchquality"
	"github.com/Shpuntyara/wms-platform/internal/modules"
	"github.com/Shpuntyara/wms-platform/internal/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
)

func (f *fixture) cleanupExec(sql string, args ...any) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := f.admin.Exec(ctx, sql, args...); err != nil {
		f.t.Errorf("cleanup integration fixture: %v", err)
	}
}

func TestIntegrationDSNsStayInOneLocalDatabase(t *testing.T) {
	local := "host=127.0.0.1 port=55487 dbname=wms_kernel_test_isolated sslmode=disable user=admin"
	for _, tc := range []struct {
		name, admin, runtime string
		valid                bool
	}{
		{name: "isolated", admin: local, runtime: local + " user=runtime", valid: true},
		{name: "remote fallback", admin: strings.Replace(local, "127.0.0.1", "127.0.0.1,remote.invalid", 1), runtime: local},
		{name: "different database", admin: local, runtime: strings.ReplaceAll(local, "isolated", "another")},
		{name: "different port", admin: local, runtime: strings.ReplaceAll(local, "55487", "55488")},
		{name: "remote primary", admin: local, runtime: strings.ReplaceAll(local, "127.0.0.1", "remote.invalid")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateTestDatabaseDSNs(tc.admin, tc.runtime); (err == nil) != tc.valid {
				t.Fatalf("error=%v, want valid=%v", err, tc.valid)
			}
		})
	}
}

func validateTestDatabaseDSNs(adminDSN, runtimeDSN string) error {
	var database string
	var port uint16
	for _, dsn := range []string{adminDSN, runtimeDSN} {
		config, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			return errors.New("invalid integration database configuration")
		}
		if config.ConnConfig.Host != "127.0.0.1" || !strings.HasPrefix(config.ConnConfig.Database, "wms_kernel_test") {
			return errors.New("refusing a non-isolated integration database")
		}
		for _, fallback := range config.ConnConfig.Fallbacks {
			if fallback.Host != "127.0.0.1" || fallback.Port != config.ConnConfig.Port {
				return errors.New("integration database fallback leaves the isolated server")
			}
		}
		if database != "" && (database != config.ConnConfig.Database || port != config.ConnConfig.Port) {
			return errors.New("integration database connections must target the same server and database")
		}
		database, port = config.ConnConfig.Database, config.ConnConfig.Port
	}
	return nil
}

func TestStoreRejectsDirectCoreWrites(t *testing.T) {
	f := newFixture(t)
	for _, grant := range []string{
		"UPDATE ON platform.actor_grants", "UPDATE ON inventory.reservations",
		"UPDATE ON warehouse.documents", "INSERT ON platform.operations",
		"TRUNCATE ON inventory.movements", "UPDATE (reserved_qty) ON inventory.stock_balances",
		"CREATE ON SCHEMA contracts_v1", "UPDATE ON contracts_v1.documents",
	} {
		t.Run(grant, func(t *testing.T) {
			f.exec("GRANT " + grant + " TO wms_go_runtime")
			t.Cleanup(func() { f.cleanupExec("REVOKE " + grant + " FROM wms_go_runtime") })
			if _, err := postgres.New(t.Context(), f.runtime); err == nil {
				t.Fatal("unsafe runtime privileges accepted")
			}
		})
	}
	t.Run("owner through SET ROLE", func(t *testing.T) {
		f.exec("GRANT wms_test_owner TO wms_go_runtime WITH INHERIT FALSE")
		t.Cleanup(func() { f.cleanupExec("REVOKE wms_test_owner FROM wms_go_runtime") })
		if _, err := postgres.New(t.Context(), f.runtime); err == nil {
			t.Fatal("runtime can become the schema owner")
		}
	})
}

// Discard the server's COMMIT acknowledgement after PostgreSQL has sent it.
// The client must treat this as unknown outcome and recover by the original ID.
type commitAckDropper struct {
	net.Conn
	dropped *atomic.Bool
	tail    []byte
}

func (c *commitAckDropper) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	combined := append(c.tail, b[:n]...)
	if bytes.Contains(combined, []byte{'C', 0, 0, 0, 11, 'C', 'O', 'M', 'M', 'I', 'T', 0}) && c.dropped.CompareAndSwap(false, true) {
		_ = c.Conn.Close()
		return 0, io.EOF
	}
	if len(combined) > 11 {
		combined = combined[len(combined)-11:]
	}
	c.tail = bytes.Clone(combined)
	return n, err
}

func TestLostCommitAcknowledgementRecoversByCommandID(t *testing.T) {
	f := newFixture(t)
	cfg, err := pgxpool.ParseConfig(os.Getenv("WMS_TEST_RUNTIME_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	var dropped atomic.Bool
	cfg.MaxConns = 1
	cfg.ConnConfig.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return &commitAckDropper{Conn: conn, dropped: &dropped}, nil
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	f.store, err = postgres.New(t.Context(), pool)
	if err != nil {
		t.Fatal(err)
	}
	h := f.handler(nil, nil)
	command := id()
	body := f.reserveBody(f.line, "2", 1)
	w := f.request(h, "POST", "/api/v1/reservations", body, command)
	if !dropped.Load() {
		t.Fatal("the COMMIT acknowledgement was not dropped")
	}
	if got := f.scalar(`SELECT count(*)::text FROM platform.operations WHERE id=$1 AND completed_at IS NOT NULL`, command); got != "1" {
		t.Fatalf("the server did not commit before the simulated disconnect: %s", got)
	}
	requireStatus(t, w, 503)
	if !strings.Contains(w.Body.String(), "same command identifier") || w.Header().Get("Retry-After") == "" {
		t.Fatalf("missing recovery instruction: %s", w.Body)
	}
	w = f.request(h, "POST", "/api/v1/reservations", body, command)
	requireStatus(t, w, 201)
	if got := f.scalar(`SELECT reserved_qty::text FROM inventory.stock_balances WHERE id=$1`, f.source); got != "2.000000" {
		t.Fatalf("recovery reserved stock twice: %s", got)
	}
}

func TestModuleInstallationIntegrity(t *testing.T) {
	f := newFixture(t)
	f.exec(`DELETE FROM platform.module_migrations WHERE module_id='example.dispatch_quality'`)
	f.exec(`DELETE FROM platform.modules WHERE id='example.dispatch_quality'`)
	// The package activation records the real migration checksums.
	activation, err := os.ReadFile("../../db/examples/enable_dispatch_quality.psql")
	if err != nil {
		t.Fatal(err)
	}
	_, sql, _ := strings.Cut(string(activation), "BEGIN;")
	f.exec("BEGIN;" + sql)
	t.Cleanup(func() {
		f.cleanupExec(`DELETE FROM platform.module_migrations WHERE module_id='example.dispatch_quality'`)
		f.cleanupExec(`DELETE FROM platform.modules WHERE id='example.dispatch_quality'`)
	})
	installed, err := f.store.InstalledModules(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	registry, err := modules.NewRegistry(installed, []modules.Module{dispatchquality.New()}, []string{dispatchquality.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.ValidateModules(t.Context(), registry); err != nil {
		t.Fatal(err)
	}
	t.Run("checksum mismatch", func(t *testing.T) {
		checksum := f.scalar(`SELECT checksum FROM platform.module_migrations WHERE module_id='example.dispatch_quality' AND migration_id='001_create_checks'`)
		f.exec(`UPDATE platform.module_migrations SET checksum=$1 WHERE module_id='example.dispatch_quality' AND migration_id='001_create_checks'`, strings.Repeat("0", 64))
		t.Cleanup(func() {
			f.exec(`UPDATE platform.module_migrations SET checksum=$1 WHERE module_id='example.dispatch_quality' AND migration_id='001_create_checks'`, checksum)
		})
		if err := f.store.ValidateModules(t.Context(), registry); err == nil {
			t.Fatal("tampered migration history accepted")
		}
	})
	t.Run("missing migration", func(t *testing.T) {
		checksum := f.scalar(`SELECT checksum FROM platform.module_migrations WHERE module_id='example.dispatch_quality' AND migration_id='002_document_facts'`)
		f.exec(`DELETE FROM platform.module_migrations WHERE module_id='example.dispatch_quality' AND migration_id='002_document_facts'`)
		t.Cleanup(func() {
			f.exec(`INSERT INTO platform.module_migrations(module_id,migration_id,checksum) VALUES('example.dispatch_quality','002_document_facts',$1)`, checksum)
		})
		if err := f.store.ValidateModules(t.Context(), registry); err == nil {
			t.Fatal("incomplete migration history accepted")
		}
	})
	t.Run("unexpected migration", func(t *testing.T) {
		f.exec(`INSERT INTO platform.module_migrations(module_id,migration_id,checksum) VALUES('example.dispatch_quality','999_unknown',$1)`, strings.Repeat("0", 64))
		t.Cleanup(func() {
			f.exec(`DELETE FROM platform.module_migrations WHERE module_id='example.dispatch_quality' AND migration_id='999_unknown'`)
		})
		if err := f.store.ValidateModules(t.Context(), registry); err == nil {
			t.Fatal("unknown future schema accepted by old module code")
		}
	})
	t.Run("schema metadata mismatch", func(t *testing.T) {
		f.exec(`UPDATE platform.modules SET schema_name='mod_wrong' WHERE id='example.dispatch_quality'`)
		t.Cleanup(func() {
			f.exec(`UPDATE platform.modules SET schema_name='mod_example_dispatch_quality' WHERE id='example.dispatch_quality'`)
		})
		if _, err := f.store.InstalledModules(t.Context()); err == nil {
			t.Fatal("foreign schema metadata accepted")
		}
	})
	t.Run("manifest schema mismatch", func(t *testing.T) {
		var manifest map[string]any
		_ = json.Unmarshal([]byte(f.scalar(`SELECT manifest::text FROM platform.modules WHERE id='example.dispatch_quality'`)), &manifest)
		f.exec(`UPDATE platform.modules SET manifest=jsonb_set(manifest,'{data,schema}','"mod_wrong"') WHERE id='example.dispatch_quality'`)
		t.Cleanup(func() {
			raw, _ := json.Marshal(manifest)
			f.exec(`UPDATE platform.modules SET manifest=$1::jsonb WHERE id='example.dispatch_quality'`, string(raw))
		})
		if _, err := f.store.InstalledModules(t.Context()); err == nil {
			t.Fatal("foreign manifest schema accepted")
		}
	})
}
