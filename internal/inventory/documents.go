package inventory

// DocumentInput creates an immutable set of planned lines or transitions an
// existing document. Inventory quantities are posted through kernel commands.
type DocumentInput struct {
	OrganizationID  string              `json:"organization_id"`
	WarehouseID     string              `json:"warehouse_id"`
	OwnerID         string              `json:"owner_id,omitempty"`
	Kind            string              `json:"kind,omitempty"`
	Number          string              `json:"number,omitempty"`
	Lines           []DocumentLineInput `json:"lines,omitempty"`
	ExpectedVersion int64               `json:"expected_document_version,omitempty"`
	State           string              `json:"state,omitempty"`
}
type DocumentLineInput struct {
	ItemID   string `json:"item_id"`
	Quantity string `json:"quantity"`
}
