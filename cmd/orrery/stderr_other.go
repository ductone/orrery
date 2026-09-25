//go:build !unix

package main

func redirectStderr(string) (func(), error) { return func() {}, nil }
