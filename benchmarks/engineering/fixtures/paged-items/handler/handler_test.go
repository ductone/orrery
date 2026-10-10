package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"benchmark/paged-items/store"
)

func TestGetItem(t *testing.T) {
	s := store.New()
	s.Add(store.Item{ID: "x", Name: "X"})
	h := New(s)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/items/x", nil))
	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/items/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404", rec.Code)
	}
}
