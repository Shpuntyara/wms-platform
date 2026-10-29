//go:build integration

package postgres_test

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"
)

func TestDocumentSparseOwnerScopeCannotEnumerateOrDuplicateOtherOwners(t *testing.T) {
	f := newFixture(t)
	owners := []string{id(), id(), id()}
	slices.Sort(owners)
	var expected []string
	for index, owner := range owners {
		document := id()
		f.exec(`INSERT INTO catalog.stock_owners(id,organization_id,code) VALUES($1,$2,$1::uuid::text)`, owner, f.org)
		f.exec(`INSERT INTO warehouse.documents(id,organization_id,warehouse_id,owner_id,kind,number,state) VALUES($1,$2,$3,$4,'receipt',$1::uuid::text,'draft')`, document, f.org, f.wh, owner)
		f.exec(`INSERT INTO warehouse.document_lines(document_id,organization_id,warehouse_id,owner_id,item_id,line_no,requested_qty) VALUES($1,$2,$3,$4,$5,1,1)`, document, f.org, f.wh, owner, f.item)
		if index < 2 {
			expected = append(expected, document)
			f.exec(`INSERT INTO platform.actor_grants(actor_id,organization_id,warehouse_id,owner_id,permission) VALUES($1,$2,$3,$4,'document.read')`, f.actor, f.org, f.wh, owner)
		}
	}
	slices.Sort(expected)
	response := f.request(f.handler(nil, nil), "GET", fmt.Sprintf("/api/v1/documents?organization_id=%s&warehouse_id=%s&limit=20", f.org, f.wh), nil, "")
	requireStatus(t, response, 200)
	var page struct {
		Data []struct {
			ID      string `json:"id"`
			OwnerID string `json:"owner_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, document := range page.Data {
		if !slices.Contains(owners[:2], document.OwnerID) {
			t.Fatalf("document for an unpermitted owner escaped its closed range: %+v", document)
		}
		got = append(got, document.ID)
	}
	if !slices.Equal(got, expected) {
		t.Fatalf("sparse owner page leaked, duplicated or omitted documents: got=%v want=%v", got, expected)
	}
}
