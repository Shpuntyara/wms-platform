package postgres

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/Shpuntyara/wms-platform/internal/inventory"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestTranslateConnectionFailurePreservesCommandRecovery(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "EOF after request", err: io.EOF},
		{name: "truncated response", err: io.ErrUnexpectedEOF},
		{name: "closed pipe", err: io.ErrClosedPipe},
		{name: "closed socket", err: net.ErrClosed},
		{name: "closed pgx connection", err: pgconn.ErrConnClosed},
		{name: "network operation", err: &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset")}},
		{name: "SQL connection failure", err: &pgconn.PgError{Code: "08006", Message: "private connection details"}},
		{name: "SQL connection disappeared", err: &pgconn.PgError{Code: "08003"}},
		{name: "unexpected commit rollback", err: pgx.ErrTxCommitRollback},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			result := translate(fmt.Errorf("database operation: %w", tc.err))
			var problem *inventory.Problem
			if !errors.As(result, &problem) || problem.Status != 503 {
				t.Fatalf("want recoverable HTTP 503 problem, got %v", result)
			}
			if !strings.Contains(problem.Detail, "same command identifier") {
				t.Fatalf("missing command recovery guidance: %s", problem.Detail)
			}
			if strings.Contains(problem.Detail, "private") {
				t.Fatalf("connection details leaked: %s", problem.Detail)
			}
		})
	}
}
