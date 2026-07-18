package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestModelLister(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Fatalf("authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"z"},{"id":"a"},{"id":"a"},{"id":""}]}`))
	}))
	defer server.Close()
	models, err := NewModelLister(server.Client()).List(context.Background(), server.URL+"/v1", "secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(models, []string{"a", "z"}) {
		t.Fatalf("models = %#v", models)
	}
}
