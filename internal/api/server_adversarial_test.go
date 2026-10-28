package api

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Shpuntyara/wms-platform/internal/inventory"
)

type deadlineRecordingBackend struct {
	*fakeBackend
	deadline    time.Time
	hasDeadline bool
}

func (b *deadlineRecordingBackend) List(ctx context.Context, _ inventory.Resource, _ inventory.Scope, _ inventory.Filters) (inventory.Page, error) {
	b.deadline, b.hasDeadline = ctx.Deadline()
	return inventory.Page{}, nil
}

func TestRequestDeadlineIsShorterThanTheCallerContext(t *testing.T) {
	h, backend := testHandler(t)
	recorder := &deadlineRecordingBackend{fakeBackend: backend}
	s := h.(*Server)
	s.config.Backend = recorder
	s.config.RequestTimeout = 2 * time.Second
	parent, cancel := context.WithTimeout(t.Context(), time.Hour)
	defer cancel()
	r := httptest.NewRequest("GET", "/api/v1/stock-balances?organization_id="+testOrg+"&warehouse_id="+testWarehouse, nil).WithContext(parent)
	r.Header.Set("Authorization", "Bearer valid")
	w := httptest.NewRecorder()
	before := time.Now()
	h.ServeHTTP(w, r)
	if w.Code != 200 || !recorder.hasDeadline || recorder.deadline.After(before.Add(3*time.Second)) || recorder.deadline.Before(before.Add(time.Second)) {
		t.Fatalf("backend deadline=%v present=%v status=%d", recorder.deadline, recorder.hasDeadline, w.Code)
	}
}

func TestRequestBodyLimitIncludesTrailingJSONAndWhitespace(t *testing.T) {
	valid := `{"organization_id":"` + testOrg + `","warehouse_id":"` + testWarehouse + `","document_line_id":"` + testLine + `","expected_document_version":1,"balance_id":"` + testBalance + `","quantity":"1"}`
	for _, tc := range []struct {
		name, suffix string
		status       int
	}{
		{name: "small whitespace remains valid", suffix: strings.Repeat(" ", 1024), status: 201},
		{name: "small second value is rejected", suffix: " {}", status: 400},
		{name: "oversized trailing whitespace", suffix: strings.Repeat(" ", 1024*1024), status: 413},
		{name: "oversized second value", suffix: ` {"padding":"` + strings.Repeat("x", 1024*1024) + `"}`, status: 413},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, backend := testHandler(t)
			w := request(h, "POST", "/api/v1/reservations", valid+tc.suffix, "valid", testCommand, "application/json")
			if w.Code != tc.status {
				t.Fatalf("status=%d want=%d body=%s", w.Code, tc.status, w.Body)
			}
			if tc.status != 201 && backend.calls != 0 {
				t.Fatal("invalid or oversized body reached the backend")
			}
			if tc.status == 413 && !strings.Contains(w.Body.String(), `"code":"request_too_large"`) {
				t.Fatalf("wrong oversized-body problem: %s", w.Body)
			}
		})
	}
}
