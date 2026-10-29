package api

import (
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Shpuntyara/wms-platform/internal/inventory"
)

func (s *Server) documentCommand(w http.ResponseWriter, r *http.Request, transition bool) {
	command, err := inventory.NormalizeID(r.Header.Get("Idempotency-Key"))
	if err != nil {
		s.problem(w, r, inventory.Invalid("invalid_idempotency_key"))
		return
	}
	var input inventory.DocumentInput
	if err := decode(w, r, &input); err != nil {
		s.problem(w, r, err)
		return
	}
	for _, field := range []*string{&input.OrganizationID, &input.WarehouseID} {
		value, err := inventory.NormalizeID(*field)
		if err != nil {
			s.problem(w, r, err)
			return
		}
		*field = value
	}
	document := ""
	permission := "document.create"
	if transition {
		document, err = inventory.NormalizeID(r.PathValue("id"))
		if err == nil && (input.ExpectedVersion < 1 || (input.State != "active" && input.State != "closed" && input.State != "cancelled") || input.OwnerID != "" || input.Kind != "" || input.Number != "" || input.Recipient != "" || input.OrderReference != "" || input.Lines != nil) {
			err = inventory.Invalid("invalid_document_transition")
		}
		permission = "document.transition"
	} else {
		input.OwnerID, err = inventory.NormalizeID(input.OwnerID)
		automaticNumber := input.Number == "" && (input.Kind == "receipt" || input.Kind == "transfer" || input.Kind == "shipment")
		if err == nil && (input.ExpectedVersion != 0 || input.State != "" || len(input.Lines) < 1 || len(input.Lines) > 200 || (!automaticNumber && strings.TrimSpace(input.Number) == "") || utf8.RuneCountInString(input.Number) > 100 || (input.Kind != "receipt" && input.Kind != "shipment" && input.Kind != "transfer" && input.Kind != "adjustment")) {
			err = inventory.Invalid("invalid_document_input")
		}
		if err == nil && (!documentLabel(input.Recipient, 200) || !documentLabel(input.OrderReference, 100) || (input.Kind == "shipment" && strings.TrimSpace(input.Recipient) == "")) {
			err = inventory.Invalid("invalid_document_recipient")
		}
		if err == nil {
			for i := range input.Lines {
				line := &input.Lines[i]
				line.ItemID, err = inventory.NormalizeID(line.ItemID)
				if err != nil {
					break
				}
				err = inventory.ValidateQuantity(line.Quantity)
				if err != nil {
					break
				}
			}
		}
	}
	if err != nil {
		s.problem(w, r, err)
		return
	}
	p := principal(r)
	var scope inventory.Scope
	if transition {
		scope, err = p.Scope(permission, input.OrganizationID, input.WarehouseID)
	} else {
		scope, err = input.CreationScope(p)
	}
	if err != nil {
		s.problem(w, r, err)
		return
	}
	if !transition && !scope.AllowsOwner(input.OwnerID) {
		s.problem(w, r, inventory.Forbidden())
		return
	}
	result, err := s.config.Backend.CommandDocument(r.Context(), p, command, document, input, s.config.Modules)
	if err != nil {
		s.problem(w, r, err)
		return
	}
	writeJSON(w, 201, map[string]any{"data": result})
}

func documentLabel(value string, maximum int) bool {
	return utf8.RuneCountInString(value) <= maximum && !strings.ContainsFunc(value, unicode.IsControl)
}
