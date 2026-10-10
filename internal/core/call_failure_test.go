package core

import (
	"errors"
	"fmt"
	"syscall"
	"testing"

	"github.com/ductone/orrey/internal/config"
	"github.com/ductone/orrey/internal/model"
	"github.com/ductone/orrey/internal/provider"
)

func TestClassifyCallError(t *testing.T) {
	providers := provider.New(config.Config{})
	m := model.ModelSpec{ID: "ramp/x"}
	cases := []struct {
		err  error
		want failureClass
	}{
		{&provider.MalformedToolArgumentsError{Name: "edit", Err: errors.New("unexpected end of JSON input")}, failMalformed},
		{&provider.HTTPError{Status: 404, Body: `{"error":{"code":"model_not_found"}}`}, failRefused},
		{&provider.HTTPError{Status: 400, Body: `{"error":{"message":"bad history"}}`}, failRejected},
		{&provider.HTTPError{Status: 401, Body: "invalid key"}, failFatal},
		{fmt.Errorf("post: %w", syscall.ECONNRESET), failTransport},
		{provider.ErrCredentialsBackoff, failCooling},
		{&provider.HTTPError{Status: 503, Body: "overloaded"}, failTransient},
	}
	for _, c := range cases {
		if got := classifyCallError(c.err, providers, m); got != c.want {
			t.Errorf("%v: got %s, want %s", c.err, got, c.want)
		}
	}
	for class := range map[failureClass]bool{failMalformed: true, failRefused: true, failRejected: true, failTransport: true, failCooling: true, failTransient: true, failFatal: true} {
		if _, ok := failurePolicy[class]; !ok {
			t.Errorf("no policy for %s", class)
		}
	}
}
