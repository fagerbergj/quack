package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunServerInitAnswersWritesConfig(t *testing.T) {
	dir := t.TempDir()
	answersPath := filepath.Join(dir, "answers.yaml")
	body := "endpoint: http://localhost:11436/v1\n" +
		"main_model: qwen3.6-35b\n" +
		"session_kind: sqlite\n"
	if err := os.WriteFile(answersPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(dir, "quack.yaml")

	if err := runServerInitAnswers(answersPath, outPath, false); err != nil {
		t.Fatalf("runServerInitAnswers: %v", err)
	}
	b, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("output not written: %v", err)
	}
	if !strings.Contains(string(b), "qwen3.6-35b") {
		t.Errorf("emitted config missing the answered model:\n%s", b)
	}
}

// Headless mode has no form to ask use/overwrite/elsewhere, so it errors.
func TestRunServerInitAnswersRefusesToOverwrite(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "quack.yaml")
	if err := os.WriteFile(outPath, []byte("existing"), 0o644); err != nil {
		t.Fatal(err)
	}
	answersPath := filepath.Join(dir, "answers.yaml")
	if err := os.WriteFile(answersPath, []byte("endpoint: http://x/v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := runServerInitAnswers(answersPath, outPath, false)
	if err == nil {
		t.Fatal("expected an error when outPath already exists without --force")
	}
	got, _ := os.ReadFile(outPath)
	if string(got) != "existing" {
		t.Error("existing file must not be touched without --force")
	}

	if err := runServerInitAnswers(answersPath, outPath, true); err != nil {
		t.Fatalf("runServerInitAnswers with force: %v", err)
	}
}

func TestFriendlyTTYErr(t *testing.T) {
	rewritten := friendlyTTYErr(errRaw("huh: could not open a new TTY: open /dev/tty: no such device or address"))
	if rewritten == nil || !strings.Contains(rewritten.Error(), "--answers") {
		t.Errorf("friendlyTTYErr = %v, want it to point at --answers", rewritten)
	}
	if friendlyTTYErr(nil) != nil {
		t.Error("friendlyTTYErr(nil) should stay nil")
	}
}

type errRaw string

func (e errRaw) Error() string { return string(e) }
