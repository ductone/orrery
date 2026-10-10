package backoff

import (
	"testing"
	"time"
)

func TestConstant(t *testing.T) {
	f := Constant(time.Second)
	if f(0) != time.Second || f(9) != time.Second {
		t.Fatal("constant schedule changed")
	}
}
