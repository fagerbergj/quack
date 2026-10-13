package main

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/fagerbergj/quack/internal/cli"
	"github.com/fagerbergj/quack/internal/config"
)

func TestRunDatasetExport_RequiresChatRepoOrSince(t *testing.T) {
	cmd := &cobra.Command{}
	err := runDatasetExport(cmd, cli.ExportOpts{Dataset: "my-dataset"}, "")
	if err == nil {
		t.Fatal("want an error when none of --chat, --repo or --since is given")
	}
}

func TestRunDatasetExport_BadSinceFlag(t *testing.T) {
	cmd := &cobra.Command{}
	err := runDatasetExport(cmd, cli.ExportOpts{Repo: "acme/widget", Dataset: "my-dataset"}, "not-a-date")
	if err == nil {
		t.Fatal("want an error for an unparseable --since")
	}
}

// --since alone selects every chat since then, so it gets past the selector check to the stores.
func TestRunDatasetExport_SinceAloneSelects(t *testing.T) {
	t.Setenv("QUACK_CONFIG", t.TempDir()+"/does-not-exist.yaml")
	err := runDatasetExport(&cobra.Command{}, cli.ExportOpts{Dataset: "my-dataset"}, "2026-01-02")
	if err == nil || strings.Contains(err.Error(), "is required") {
		t.Fatalf("err = %v, want --since alone accepted (then failing on the missing quack.yaml)", err)
	}
}

func TestRunDatasetExport_MissingConfigDegrades(t *testing.T) {
	t.Setenv("QUACK_CONFIG", t.TempDir()+"/does-not-exist.yaml")
	cmd := &cobra.Command{}
	if err := runDatasetExport(cmd, cli.ExportOpts{ChatID: "chat-1", Dataset: "my-dataset"}, ""); err == nil {
		t.Fatal("want an error when openLedgerAndStores has no local quack.yaml")
	}
}

func TestParseDateFlag(t *testing.T) {
	if got, err := parseDateFlag("2026-01-02"); err != nil || got.Year() != 2026 {
		t.Fatalf("bare date: got=%v err=%v", got, err)
	}
	if got, err := parseDateFlag("2026-01-02T03:04:05Z"); err != nil || got.Hour() != 3 {
		t.Fatalf("RFC3339: got=%v err=%v", got, err)
	}
	if _, err := parseDateFlag("not-a-date"); err == nil {
		t.Fatal("want an error for an unparseable date")
	}
}

func TestNewDatasetCmd_Tree(t *testing.T) {
	c := newDatasetCmd()
	export, _, err := c.Find([]string{"export"})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if export.Use != "export" {
		t.Fatalf("Use = %q", export.Use)
	}
}

func TestLangfuseGenClientFromConfig_RequiresLangfuseStore(t *testing.T) {
	cfg := &config.Config{Prompts: config.PromptsConfig{Store: "lf"}, Stores: map[string]config.StoreConfig{
		"lf": {Kind: "postgres", URL: "postgres://x"},
	}}
	if _, err := langfuseClientFromConfig(cfg); err == nil {
		t.Fatal("want an error when prompts.store isn't a langfuse store")
	}

	cfg.Stores["lf"] = config.StoreConfig{Kind: "langfuse", URL: "http://example.invalid", PublicKey: "pk", SecretKey: "sk"}
	if _, err := langfuseClientFromConfig(cfg); err != nil {
		t.Fatalf("want a client built from a valid langfuse store, got %v", err)
	}

	// experiments.store, when set, is the one used: a bad one fails even with a good prompts.store.
	cfg.Experiments.Store = "missing"
	if _, err := langfuseClientFromConfig(cfg); err == nil || !strings.Contains(err.Error(), `"missing"`) {
		t.Fatalf("err = %v, want experiments.store's store named", err)
	}
}
