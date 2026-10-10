package config

import "testing"

func TestParse(t *testing.T) {
	s := Parse("endpoint = https://api.example.com\nretry_limit = 5\n")
	if s.Endpoint != "https://api.example.com" || s.RetryLimit != 5 {
		t.Fatalf("settings = %+v", s)
	}
	if Parse("").RetryLimit != 3 {
		t.Fatal("retry_limit defaults to 3")
	}
}
