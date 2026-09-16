package main

import (
	"testing"

	"github.com/spf13/cobra"

	"github.com/fagerbergj/quack/internal/config"
)

func TestRunDatasetExport_RequiresChatOrRepo(t *testing.T) {
	cmd := &cobra.Command{}
	err := runDatasetExport(cmd, "", "", "", "my-dataset", 0)
	if err == nil {
		t.Fatal("want an error when neither --chat nor --repo is given")
	}
}

func TestRunDatasetExport_BadSinceFlag(t *testing.T) {
	cmd := &cobra.Command{}
	err := runDatasetExport(cmd, "", "acme/widget", "not-a-date", "my-dataset", 0)
	if err == nil {
		t.Fatal("want an error for an unparseable --since")
	}
}

func TestRunDatasetExport_MissingConfigDegrades(t *testing.T) {
	t.Setenv("QUACK_CONFIG", t.TempDir()+"/does-not-exist.yaml")
	cmd := &cobra.Command{}
	if err := runDatasetExport(cmd, "chat-1", "", "", "my-dataset", 0); err == nil {
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
	if _, err := langfuseGenClientFromConfig(cfg); err == nil {
		t.Fatal("want an error when prompts.store isn't a langfuse store")
	}

	cfg.Stores["lf"] = config.StoreConfig{Kind: "langfuse", URL: "http://example.invalid", PublicKey: "pk", SecretKey: "sk"}
	if _, err := langfuseGenClientFromConfig(cfg); err != nil {
		t.Fatalf("want a client built from a valid langfuse store, got %v", err)
	}
}
