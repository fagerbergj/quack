package vetting

import (
	"encoding/json"
	"testing"

	"google.golang.org/genai"
)

// submit_verdict's declaration is part of the cached request prefix, so recalled-memory ids must stay out of it.
func TestSubmitVerdictDeclarationRoundInvariant(t *testing.T) {
	var sink verdict
	decl := func(ids []string) string {
		st, err := newSubmitVerdictTool(&sink, ids)
		if err != nil {
			t.Fatal(err)
		}
		d, ok := st.(interface{ Declaration() *genai.FunctionDeclaration })
		if !ok {
			t.Fatalf("submit_verdict has no Declaration")
		}
		b, err := json.Marshal(d.Declaration())
		if err != nil {
			t.Fatal(err)
		}
		return st.Description() + string(b)
	}
	if a, b, none := decl([]string{"m1", "m2"}), decl([]string{"m1", "m2", "m3"}), decl(nil); a != b || a != none {
		t.Fatalf("submit_verdict declaration varies with recalled-memory ids:\n%s\n%s\n%s", a, b, none)
	}
}
