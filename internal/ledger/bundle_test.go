package ledger_test

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/ledgertest"
)

// TestAssembleBundleRoundTrip: only observation entries reach the bundle,
// as one Entry per line, with a versioned manifest.
func TestAssembleBundleRoundTrip(t *testing.T) {
	s := ledgertest.NewMemStore()
	ctx := context.Background()
	for _, kind := range []string{ledger.KindArtifactRevision, ledger.KindLLMCall, ledger.KindToolCall} {
		if _, err := s.AppendIntent(ctx, ledger.Entry{ChatID: "chat-1", Kind: kind, Payload: json.RawMessage(`{"k":1}`)}); err != nil {
			t.Fatal(err)
		}
	}

	var buf bytes.Buffer
	if err := ledger.AssembleBundle(ctx, s, "chat-1", "v1.2.3", &buf); err != nil {
		t.Fatalf("AssembleBundle: %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("zip reader: %v", err)
	}
	files := map[string][]byte{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		var b bytes.Buffer
		b.ReadFrom(rc)
		rc.Close()
		files[f.Name] = b.Bytes()
	}

	var mf ledger.Manifest
	if err := json.Unmarshal(files["manifest.json"], &mf); err != nil {
		t.Fatalf("manifest.json: %v", err)
	}
	if mf.QuackVersion != "v1.2.3" || mf.LedgerVersion != ledger.LedgerVersion || mf.SessionID != "chat-1" {
		t.Errorf("manifest = %+v", mf)
	}
	var kinds []string
	sc := bufio.NewScanner(bytes.NewReader(files["entries.jsonl"]))
	for sc.Scan() {
		var e ledger.Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("entry line: %v", err)
		}
		kinds = append(kinds, e.Kind)
	}
	if len(kinds) != 2 || kinds[0] != ledger.KindLLMCall || kinds[1] != ledger.KindToolCall {
		t.Errorf("entries.jsonl kinds = %v, want the two observation kinds only", kinds)
	}
}

// TestReadObservationsNoRecording: intents alone are not a recording.
func TestReadObservationsNoRecording(t *testing.T) {
	s := ledgertest.NewMemStore()
	ctx := context.Background()
	if _, err := s.AppendIntent(ctx, ledger.Entry{ChatID: "c", Kind: ledger.KindNodeStarted}); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.ReadObservations(ctx, s, "c"); !errors.Is(err, ledger.ErrNoRecording) {
		t.Fatalf("err = %v, want ErrNoRecording", err)
	}
}
