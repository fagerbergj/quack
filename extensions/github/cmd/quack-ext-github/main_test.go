package main

import (
	"os"
	"testing"
)

// main serves the registered github factory over stdio and returns cleanly when quack closes stdin.
func TestMainExitsOnStdinEOF(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	stdin, stdout := os.Stdin, os.Stdout
	t.Cleanup(func() { os.Stdin, os.Stdout = stdin, stdout })
	os.Stdin = r
	main()
}
