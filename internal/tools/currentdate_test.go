package tools

import (
	"strings"
	"testing"
	"time"

	"github.com/fagerbergj/quack/internal/promptbuilder"
)

// TestCurrentDateUsesUserZone: the tool reports the configured zone and the UTC instant.
func TestCurrentDateUsesUserZone(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatal(err)
	}
	promptbuilder.SetLocation(loc)
	t.Cleanup(func() { promptbuilder.SetLocation(nil) })
	tl, err := newCurrentDate(Deps{})
	if err != nil {
		t.Fatal(err)
	}
	out, err := tl.(runnableTool).Run(newArtifactsToolCtx(), map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := out["result"].(string)
	if !strings.Contains(got, "IST (UTC+05:30); UTC ") {
		t.Errorf("current_date = %q, want IST with its offset and the UTC instant", got)
	}
}
