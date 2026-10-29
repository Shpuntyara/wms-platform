// Package inventory defines the HTTP-facing inventory contract, not SQL storage.
package inventory

import (
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"time"
)

type Resource string

const (
	DocumentResource    Resource = "documents"
	StockResource       Resource = "stock-balances"
	ReservationResource Resource = "reservations"
	MovementResource    Resource = "movements"
	OperationResource   Resource = "operations"
)

type Problem struct {
	Status       int
	Code, Detail string
}

func (p *Problem) Error() string { return p.Code }
func Invalid(code string) error {
	return &Problem{Status: 400, Code: code, Detail: "The request contains an invalid value."}
}
func Missing() error {
	return &Problem{Status: 404, Code: "resource_not_found", Detail: "The resource was not found in the permitted scope."}
}
func Forbidden() error {
	return &Problem{Status: 403, Code: "forbidden", Detail: "The requested operation is not permitted in this scope."}
}

var quantityPattern = regexp.MustCompile(`^(0|[1-9][0-9]{0,17})(\.[0-9]{1,6})?$`)
var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func ValidateQuantity(value string) error {
	if !quantityPattern.MatchString(value) || strings.Trim(value, "0.") == "" {
		return Invalid("invalid_quantity")
	}
	return nil
}
func NormalizeID(value string) (string, error) {
	if !uuidPattern.MatchString(value) || strings.ReplaceAll(value, "-", "") == strings.Repeat("0", 32) {
		return "", Invalid("invalid_identifier")
	}
	return strings.ToLower(value), nil
}

type Grant struct{ OrganizationID, WarehouseID, OwnerID, Permission string }
type Principal struct {
	ActorID string
	Grants  []Grant
}
type Scope struct {
	ActorID, OrganizationID, WarehouseID string
	AllOwners                            bool
	OwnerIDs                             []string
}

func (p Principal) Scope(permission, org, warehouse string) (Scope, error) {
	s := Scope{ActorID: p.ActorID, OrganizationID: org, WarehouseID: warehouse}
	for _, g := range p.Grants {
		if g.Permission != permission || g.OrganizationID != org || g.WarehouseID != warehouse {
			continue
		}
		if g.OwnerID == "" {
			s.AllOwners = true
		} else {
			s.OwnerIDs = append(s.OwnerIDs, g.OwnerID)
		}
	}
	if !s.AllOwners && len(s.OwnerIDs) == 0 {
		return Scope{}, Forbidden()
	}
	slices.Sort(s.OwnerIDs)
	s.OwnerIDs = slices.Compact(s.OwnerIDs)
	return s, nil
}
func (s Scope) AllowsOwner(owner string) bool {
	return s.AllOwners || slices.Contains(s.OwnerIDs, owner)
}

type Filters struct {
	Limit                                               int
	Cursor, OwnerID, ItemID, PositionID, DocumentLineID string
	View                                                string
}
type Page struct {
	Data       []json.RawMessage
	NextCursor string
}
type Command struct {
	ID, Kind, OrganizationID, WarehouseID, DocumentLineID                                                    string
	ExpectedDocumentVersion                                                                                  int64
	BalanceID, SourceBalanceID, TargetBalanceID, MovementKind, Quantity, ReservationID, Reason, CorrectionOf string
	ExpiresAt                                                                                                *time.Time
}

func (c Command) Permission() string {
	if c.Kind == "movement" && strings.HasPrefix(c.MovementKind, "adjustment_") {
		return "movement.adjust"
	}
	switch c.Kind {
	case "reserve":
		return "reservation.create"
	case "release":
		return "reservation.release"
	default:
		return "movement.create"
	}
}
func (r Resource) ReadPermission() string {
	switch r {
	case DocumentResource:
		return "document.read"
	case StockResource:
		return "stock.read"
	case ReservationResource:
		return "reservation.read"
	case MovementResource:
		return "movement.read"
	default:
		return "operation.read"
	}
}
