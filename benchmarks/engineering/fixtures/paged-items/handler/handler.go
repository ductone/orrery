// Package handler exposes the store over HTTP.
package handler

import (
	"encoding/json"
	"net/http"

	"benchmark/paged-items/store"
)

// New returns the HTTP handler for s.
func New(s *store.Store) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /items/{id}", func(w http.ResponseWriter, r *http.Request) {
		it, ok := s.Get(r.PathValue("id"))
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(it)
	})
	return mux
}
