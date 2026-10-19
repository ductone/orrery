package store

import (
	"path/filepath"
	"testing"

	"github.com/ductone/orrery/storage"
	"github.com/ductone/orrery/storage/storagetest"
)

func TestConformance(t *testing.T) {
	storagetest.Run(t, func(t *testing.T) storage.Store {
		s, err := Open(filepath.Join(t.TempDir(), "orrery.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	})
}
