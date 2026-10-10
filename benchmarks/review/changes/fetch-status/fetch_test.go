package fetcher

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("hello"))
	}))
	defer srv.Close()
	got, err := Fetch(srv.Client(), srv.URL)
	if err != nil || got != "hello" {
		t.Fatalf("got %q err %v", got, err)
	}
}

func TestFetchBadStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer srv.Close()
	if _, err := Fetch(srv.Client(), srv.URL); err == nil {
		t.Fatal("want error for 404")
	}
}
