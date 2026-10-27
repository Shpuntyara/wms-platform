// Package api exposes the scoped inventory API and consistent HTTP problems.
package api

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Shpuntyara/wms-platform/internal/auth"
	"github.com/Shpuntyara/wms-platform/internal/inventory"
	"github.com/Shpuntyara/wms-platform/internal/modules"
)

type Backend interface {
	ResolvePrincipal(context.Context, auth.Identity) (inventory.Principal, error)
	List(context.Context, inventory.Resource, inventory.Scope, inventory.Filters) (inventory.Page, error)
	Get(context.Context, inventory.Resource, inventory.Scope, string) (json.RawMessage, error)
	Execute(context.Context, inventory.Principal, inventory.Command, *modules.Registry) (json.RawMessage, error)
	CommandDocument(context.Context, inventory.Principal, string, string, inventory.DocumentInput, *modules.Registry) (json.RawMessage, error)
	Ping(context.Context) error
}
type Config struct {
	Backend        Backend
	Auth           auth.Authenticator
	Modules        *modules.Registry
	RequestTimeout time.Duration
	Logger         *slog.Logger
}
type Server struct {
	config Config
	mux    *http.ServeMux
}
type principalKey struct{}

func New(config Config) (http.Handler, error) {
	if config.Backend == nil || config.Auth == nil || config.Modules == nil {
		return nil, errors.New("backend, authenticator and module registry are required")
	}
	if config.RequestTimeout == 0 {
		config.RequestTimeout = 5 * time.Second
	}
	if config.Logger == nil {
		config.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	s := &Server{config: config, mux: http.NewServeMux()}
	s.mux.HandleFunc("/livez", func(w http.ResponseWriter, r *http.Request) {
		if !s.method(w, r, "GET") {
			return
		}
		writeJSON(w, 200, map[string]string{"status": "alive"})
	})
	s.mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if !s.method(w, r, "GET") {
			return
		}
		if err := s.config.Backend.Ping(r.Context()); err != nil {
			s.problem(w, r, &inventory.Problem{Status: 503, Code: "not_ready", Detail: "A required dependency is unavailable."})
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ready"})
	})
	for _, resource := range []inventory.Resource{inventory.StockResource, inventory.ReservationResource, inventory.MovementResource} {
		s.mux.HandleFunc("/api/v1/"+string(resource), func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				s.list(w, r, resource)
				return
			}
			if r.Method == http.MethodPost && resource != inventory.StockResource {
				s.mutate(w, r, resource, false)
				return
			}
			allowed := "GET"
			if resource != inventory.StockResource {
				allowed += ", POST"
			}
			s.method(w, r, allowed)
		})
		s.mux.HandleFunc("/api/v1/"+string(resource)+"/{id}", func(w http.ResponseWriter, r *http.Request) {
			if s.method(w, r, "GET") {
				s.get(w, r, resource)
			}
		})
	}
	s.mux.HandleFunc("/api/v1/documents", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			s.list(w, r, inventory.DocumentResource)
		case http.MethodPost:
			s.documentCommand(w, r, false)
		default:
			s.method(w, r, "GET, POST")
		}
	})
	s.mux.HandleFunc("/api/v1/documents/{id}", func(w http.ResponseWriter, r *http.Request) {
		if s.method(w, r, "GET") {
			s.get(w, r, inventory.DocumentResource)
		}
	})
	s.mux.HandleFunc("/api/v1/documents/{id}/transitions", func(w http.ResponseWriter, r *http.Request) {
		if s.method(w, r, "POST") {
			s.documentCommand(w, r, true)
		}
	})
	s.mux.HandleFunc("/api/v1/reservations/{id}/releases", func(w http.ResponseWriter, r *http.Request) {
		if s.method(w, r, "POST") {
			s.mutate(w, r, inventory.ReservationResource, true)
		}
	})
	s.mux.HandleFunc("/api/v1/operations/{id}", func(w http.ResponseWriter, r *http.Request) {
		if s.method(w, r, "GET") {
			s.get(w, r, inventory.OperationResource)
		}
	})
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { s.problem(w, r, inventory.Missing()) })
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Request-ID", rand.Text())
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	defer func() {
		if recover() != nil {
			s.problem(w, r, errors.New("request panic"))
		}
	}()
	ctx, cancel := context.WithTimeout(r.Context(), s.config.RequestTimeout)
	defer cancel()
	r = r.WithContext(ctx)
	if r.URL.Path != "/livez" && r.URL.Path != "/readyz" {
		parts := strings.Fields(r.Header.Get("Authorization"))
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || len(parts[1]) > 16384 {
			s.problem(w, r, auth.ErrUnauthenticated)
			return
		}
		identity, err := s.config.Auth.Authenticate(ctx, parts[1])
		if err != nil {
			s.problem(w, r, auth.ErrUnauthenticated)
			return
		}
		principal, err := s.config.Backend.ResolvePrincipal(ctx, identity)
		if err != nil {
			s.problem(w, r, err)
			return
		}
		r = r.WithContext(context.WithValue(ctx, principalKey{}, principal))
	}
	s.mux.ServeHTTP(w, r)
}

func (s *Server) method(w http.ResponseWriter, r *http.Request, allowed string) bool {
	for _, method := range strings.Split(allowed, ", ") {
		if r.Method == method {
			return true
		}
	}
	w.Header().Set("Allow", allowed)
	s.problem(w, r, &inventory.Problem{Status: 405, Code: "method_not_allowed", Detail: "The method is not supported for this resource."})
	return false
}

func principal(r *http.Request) inventory.Principal {
	p, _ := r.Context().Value(principalKey{}).(inventory.Principal)
	return p
}

func queryScope(r *http.Request) (string, string, error) {
	for _, values := range r.URL.Query() {
		if len(values) != 1 {
			return "", "", inventory.Invalid("duplicate_query_parameter")
		}
	}
	org, err := inventory.NormalizeID(r.URL.Query().Get("organization_id"))
	if err != nil {
		return "", "", err
	}
	wh, err := inventory.NormalizeID(r.URL.Query().Get("warehouse_id"))
	return org, wh, err
}

func (s *Server) list(w http.ResponseWriter, r *http.Request, resource inventory.Resource) {
	org, wh, err := queryScope(r)
	if err != nil {
		s.problem(w, r, err)
		return
	}
	scope, err := principal(r).Scope(resource.ReadPermission(), org, wh)
	if err != nil {
		s.problem(w, r, err)
		return
	}
	f := inventory.Filters{Limit: 50, Cursor: r.URL.Query().Get("cursor")}
	if len(f.Cursor) > 2048 {
		s.problem(w, r, inventory.Invalid("invalid_cursor"))
		return
	}
	if limit := r.URL.Query().Get("limit"); limit != "" {
		f.Limit, err = strconv.Atoi(limit)
		if err != nil || f.Limit < 1 || f.Limit > 200 {
			s.problem(w, r, inventory.Invalid("invalid_limit"))
			return
		}
	}
	for _, entry := range []struct {
		key  string
		dest *string
	}{{"owner_id", &f.OwnerID}, {"item_id", &f.ItemID}, {"position_id", &f.PositionID}, {"document_line_id", &f.DocumentLineID}} {
		if value := r.URL.Query().Get(entry.key); value != "" {
			*entry.dest, err = inventory.NormalizeID(value)
			if err != nil {
				s.problem(w, r, err)
				return
			}
		}
	}
	page, err := s.config.Backend.List(r.Context(), resource, scope, f)
	if err != nil {
		s.problem(w, r, err)
		return
	}
	var next any
	if page.NextCursor != "" {
		next = page.NextCursor
	}
	if page.Data == nil {
		page.Data = []json.RawMessage{}
	}
	writeJSON(w, 200, map[string]any{"data": page.Data, "pagination": map[string]any{"next_cursor": next}})
}

func (s *Server) get(w http.ResponseWriter, r *http.Request, resource inventory.Resource) {
	org, wh, err := queryScope(r)
	if err != nil {
		s.problem(w, r, err)
		return
	}
	id, err := inventory.NormalizeID(r.PathValue("id"))
	if err != nil {
		s.problem(w, r, err)
		return
	}
	scope, err := principal(r).Scope(resource.ReadPermission(), org, wh)
	if err != nil {
		s.problem(w, r, err)
		return
	}
	result, err := s.config.Backend.Get(r.Context(), resource, scope, id)
	if err != nil {
		s.problem(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"data": result})
}

type scopeInput struct {
	OrganizationID  string `json:"organization_id"`
	WarehouseID     string `json:"warehouse_id"`
	ExpectedVersion int64  `json:"expected_document_version"`
	Quantity        string `json:"quantity"`
}
type reserveInput struct {
	scopeInput
	DocumentLineID string     `json:"document_line_id"`
	BalanceID      string     `json:"balance_id"`
	ExpiresAt      *time.Time `json:"expires_at"`
}
type releaseInput struct {
	scopeInput
	Reason string `json:"reason"`
}
type movementInput struct {
	scopeInput
	DocumentLineID string `json:"document_line_id"`
	Kind           string `json:"kind"`
	Source         string `json:"source_balance_id"`
	Target         string `json:"target_balance_id"`
	Reservation    string `json:"reservation_id"`
	Reason         string `json:"reason"`
	CorrectionOf   string `json:"correction_of"`
}

func decode(w http.ResponseWriter, r *http.Request, dest any) error {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		return &inventory.Problem{Status: 415, Code: "unsupported_media_type", Detail: "Use application/json."}
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1024*1024)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dest); err != nil {
		var large *http.MaxBytesError
		if errors.As(err, &large) {
			return &inventory.Problem{Status: 413, Code: "request_too_large", Detail: "The request exceeds the size limit."}
		}
		return inventory.Invalid("invalid_json")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return inventory.Invalid("trailing_json")
	}
	return nil
}

func (s *Server) mutate(w http.ResponseWriter, r *http.Request, resource inventory.Resource, release bool) {
	c := inventory.Command{}
	var input scopeInput
	var err error
	c.ID, err = inventory.NormalizeID(r.Header.Get("Idempotency-Key"))
	if err != nil {
		s.problem(w, r, inventory.Invalid("invalid_idempotency_key"))
		return
	}
	switch {
	case release:
		var body releaseInput
		if err = decode(w, r, &body); err != nil {
			break
		}
		input = body.scopeInput
		c.Kind = "release"
		c.Reason = body.Reason
		c.ReservationID = r.PathValue("id")
	case resource == inventory.ReservationResource:
		var body reserveInput
		if err = decode(w, r, &body); err != nil {
			break
		}
		input = body.scopeInput
		c.Kind = "reserve"
		c.DocumentLineID = body.DocumentLineID
		c.BalanceID = body.BalanceID
		c.ExpiresAt = body.ExpiresAt
	default:
		var body movementInput
		if err = decode(w, r, &body); err != nil {
			break
		}
		input = body.scopeInput
		c.Kind = "movement"
		c.DocumentLineID = body.DocumentLineID
		c.MovementKind = body.Kind
		c.SourceBalanceID = body.Source
		c.TargetBalanceID = body.Target
		c.ReservationID = body.Reservation
		c.Reason = body.Reason
		c.CorrectionOf = body.CorrectionOf
	}
	if err != nil {
		s.problem(w, r, err)
		return
	}
	c.OrganizationID = input.OrganizationID
	c.WarehouseID = input.WarehouseID
	c.ExpectedDocumentVersion = input.ExpectedVersion
	c.Quantity = input.Quantity
	if err := validateCommand(&c); err != nil {
		s.problem(w, r, err)
		return
	}
	p := principal(r)
	if _, err := p.Scope(c.Permission(), c.OrganizationID, c.WarehouseID); err != nil {
		s.problem(w, r, err)
		return
	}
	result, err := s.config.Backend.Execute(r.Context(), p, c, s.config.Modules)
	if err != nil {
		s.problem(w, r, err)
		return
	}
	var ids struct {
		Reservation string `json:"reservation_id"`
		Movement    string `json:"movement_id"`
	}
	_ = json.Unmarshal(result, &ids)
	resourceID := ids.Reservation
	if resource == inventory.MovementResource {
		resourceID = ids.Movement
	}
	if resourceID != "" {
		w.Header().Set("Location", "/api/v1/"+string(resource)+"/"+resourceID+"?organization_id="+c.OrganizationID+"&warehouse_id="+c.WarehouseID)
	}
	writeJSON(w, 201, map[string]any{"data": result})
}

func validateCommand(c *inventory.Command) error {
	for _, field := range []*string{&c.OrganizationID, &c.WarehouseID} {
		value, err := inventory.NormalizeID(*field)
		if err != nil {
			return err
		}
		*field = value
	}
	if c.ExpectedDocumentVersion < 1 {
		return inventory.Invalid("invalid_expected_document_version")
	}
	if err := inventory.ValidateQuantity(c.Quantity); err != nil {
		return err
	}
	for _, field := range []*string{&c.DocumentLineID, &c.BalanceID, &c.SourceBalanceID, &c.TargetBalanceID, &c.ReservationID, &c.CorrectionOf} {
		if *field != "" {
			value, err := inventory.NormalizeID(*field)
			if err != nil {
				return err
			}
			*field = value
		}
	}
	if utf8.RuneCountInString(c.Reason) > 2000 {
		return inventory.Invalid("invalid_reason")
	}
	switch c.Kind {
	case "reserve":
		if c.DocumentLineID == "" || c.BalanceID == "" {
			return inventory.Invalid("missing_identifier")
		}
	case "release":
		if c.ReservationID == "" || strings.TrimSpace(c.Reason) == "" {
			return inventory.Invalid("invalid_release")
		}
	case "movement":
		if c.DocumentLineID == "" {
			return inventory.Invalid("missing_identifier")
		}
		switch c.MovementKind {
		case "receipt", "adjustment_in":
			if c.SourceBalanceID != "" || c.TargetBalanceID == "" || c.ReservationID != "" {
				return inventory.Invalid("invalid_movement_shape")
			}
		case "transfer":
			if c.SourceBalanceID == "" || c.TargetBalanceID == "" || c.SourceBalanceID == c.TargetBalanceID {
				return inventory.Invalid("invalid_movement_shape")
			}
		case "issue":
			if c.SourceBalanceID == "" || c.TargetBalanceID != "" || c.ReservationID == "" {
				return inventory.Invalid("invalid_movement_shape")
			}
		case "adjustment_out":
			if c.SourceBalanceID == "" || c.TargetBalanceID != "" || c.ReservationID != "" {
				return inventory.Invalid("invalid_movement_shape")
			}
		default:
			return inventory.Invalid("invalid_movement_kind")
		}
		if strings.HasPrefix(c.MovementKind, "adjustment_") {
			if strings.TrimSpace(c.Reason) == "" {
				return inventory.Invalid("adjustment_reason_required")
			}
		} else if c.CorrectionOf != "" {
			return inventory.Invalid("invalid_correction")
		}
	default:
		return inventory.Invalid("invalid_command")
	}
	return nil
}

func (s *Server) problem(w http.ResponseWriter, r *http.Request, err error) {
	status, code, detail, moduleID := 500, "internal_error", "The request could not be completed.", ""
	var p *inventory.Problem
	var denial *modules.Denial
	var unavailable *modules.Unavailable
	switch {
	case errors.Is(err, auth.ErrUnauthenticated):
		status, code, detail = 401, "unauthenticated", "A valid access token is required."
		w.Header().Set("WWW-Authenticate", `Bearer realm="wms"`)
	case errors.As(err, &p):
		status, code, detail = p.Status, p.Code, p.Detail
	case errors.As(err, &denial):
		status, code, detail, moduleID = 409, denial.Code, "A required client check did not approve this operation.", denial.ModuleID
	case errors.As(err, &unavailable):
		status, code, detail, moduleID = 503, "required_guard_unavailable", "A required client check is unavailable. The operation was not committed.", unavailable.ModuleID
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		status, code, detail = 503, "operation_interrupted", "Retry with the same command identifier to determine the result."
	}
	if status == 503 {
		w.Header().Set("Retry-After", "1")
	}
	trace := w.Header().Get("X-Request-ID")
	if status >= 500 {
		s.config.Logger.Error("request unavailable", "code", code, "trace_id", trace)
	}
	body := map[string]any{"type": "urn:wms:problem:" + code, "title": http.StatusText(status), "status": status, "code": code, "detail": detail, "trace_id": trace}
	if moduleID != "" {
		body["module_id"] = moduleID
	}
	w.Header().Set("Content-Type", "application/problem+json")
	writeJSON(w, status, body)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	bytes, err := json.Marshal(value)
	if err != nil {
		status = 500
		bytes = []byte(`{"type":"urn:wms:problem:invalid_response","title":"Internal Server Error","status":500,"code":"invalid_response"}`)
		w.Header().Set("Content-Type", "application/problem+json")
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(status)
	_, _ = w.Write(append(bytes, '\n'))
}
