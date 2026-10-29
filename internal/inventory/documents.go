package inventory

// DocumentInput creates an immutable set of planned lines or transitions an
// existing document. Inventory quantities are posted through kernel commands.
type DocumentInput struct {
	OrganizationID  string              `json:"organization_id"`
	WarehouseID     string              `json:"warehouse_id"`
	OwnerID         string              `json:"owner_id,omitempty"`
	Kind            string              `json:"kind,omitempty"`
	Number          string              `json:"number,omitempty"`
	Recipient       string              `json:"recipient,omitempty"`
	OrderReference  string              `json:"order_reference,omitempty"`
	Lines           []DocumentLineInput `json:"lines,omitempty"`
	ExpectedVersion int64               `json:"expected_document_version,omitempty"`
	State           string              `json:"state,omitempty"`
}

// CreationScope keeps general document creation and owner-bound warehouse
// issue/move permissions separate. Neither warehouse permission admits PZ.
func (input DocumentInput) CreationScope(p Principal) (Scope, error) {
	if scope, err := p.Scope("document.create", input.OrganizationID, input.WarehouseID); err == nil && scope.AllowsOwner(input.OwnerID) {
		return scope, nil
	}
	permission := ""
	switch input.Kind {
	case "shipment":
		permission = "document.issue"
	case "transfer":
		permission = "document.move"
	default:
		return Scope{}, Forbidden()
	}
	scope, err := p.Scope(permission, input.OrganizationID, input.WarehouseID)
	if err != nil || !scope.AllowsOwner(input.OwnerID) {
		return Scope{}, Forbidden()
	}
	return scope, nil
}

type DocumentLineInput struct {
	ItemID   string `json:"item_id"`
	Quantity string `json:"quantity"`
}
