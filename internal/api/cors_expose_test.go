package api

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
)

// The console edits raw JSON; saving a rule must not silently discard the
// response headers the browser needs, including when editing an older rule.
func TestCORSExposeHeadersSurviveDashboardRoundTrip(t *testing.T) {
	h, store := newTestAPI(t)
	if err := store.CreateBucket("browser-cors"); err != nil {
		t.Fatal(err)
	}
	token := getToken(t, h)
	for _, rule := range []map[string]interface{}{
		{"allowed_origins": []string{"https://app.example.com"}, "allowed_methods": []string{"PUT"}, "expose_headers": []string{"ETag", "x-amz-version-id"}},
		{"allowed_origins": []string{"https://app.example.com"}, "allowed_methods": []string{"PUT"}},
	} {
		body := map[string]interface{}{"rules": []interface{}{rule}}
		put := doRequest(h, http.MethodPut, "/buckets/browser-cors/cors", body, token)
		if put.Code != http.StatusOK {
			t.Fatalf("PUT: %d %s", put.Code, put.Body.String())
		}
		get := doRequest(h, http.MethodGet, "/buckets/browser-cors/cors", nil, token)
		var got, want interface{}
		if err := json.Unmarshal(get.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(body)
		if err := json.Unmarshal(encoded, &want); err != nil {
			t.Fatal(err)
		}
		if get.Code != http.StatusOK || !reflect.DeepEqual(got, want) {
			t.Errorf("GET: %d %s, want %s", get.Code, get.Body.String(), encoded)
		}
	}
}
