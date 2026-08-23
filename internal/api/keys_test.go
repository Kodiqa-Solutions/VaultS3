package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

// The Access Keys page shows which application each key belongs to, and it reads
// that from the userId this endpoint returns. The field is stored but was never
// surfaced, so four keys rendered as four rows of opaque hex with no way to tell
// them apart. Pin the contract the column depends on.
func TestListKeysReturnsTheOwningUser(t *testing.T) {
	h, _ := newTestAPI(t)
	token := getToken(t, h)

	rr := doRequest(h, "POST", "/keys", map[string]interface{}{"userId": "agrotrade-app"}, token)
	if rr.Code != http.StatusOK && rr.Code != http.StatusCreated {
		t.Fatalf("create key: %d %s", rr.Code, rr.Body.String())
	}

	rr = doRequest(h, "GET", "/keys", nil, token)
	if rr.Code != http.StatusOK {
		t.Fatalf("list keys: %d %s", rr.Code, rr.Body.String())
	}
	var items []keyListItem
	if err := json.NewDecoder(rr.Body).Decode(&items); err != nil {
		t.Fatalf("decode: %v", err)
	}

	var admin, standard *keyListItem
	for i := range items {
		if items[i].IsAdmin {
			admin = &items[i]
		} else {
			standard = &items[i]
		}
	}
	if admin == nil || standard == nil {
		t.Fatalf("want one admin entry and one created key, got %d items", len(items))
	}
	if standard.UserID != "agrotrade-app" {
		t.Errorf("created key reports userId %q, want %q, so the column would be blank",
			standard.UserID, "agrotrade-app")
	}
	// The built-in admin key is not tied to a created IAM user. The page labels
	// it "Built-in" on exactly that absence, so a userId here would mislabel it.
	if admin.UserID != "" {
		t.Errorf("built-in admin entry reports userId %q, want empty", admin.UserID)
	}
}

// The JSON field name is what the dashboard binds to. Renaming it would empty the
// column silently, with nothing else failing.
func TestKeyListItemSerializesUserIDAsUserId(t *testing.T) {
	b, err := json.Marshal(keyListItem{AccessKey: "k", UserID: "app"})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got["userId"] != "app" {
		t.Errorf("serialized as %s, want a userId field", b)
	}
}
