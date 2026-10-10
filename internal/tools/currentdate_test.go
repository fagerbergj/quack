package tools

import (
	"strings"
	"testing"
	"time"

	"github.com/fagerbergj/quack/internal/promptbuilder"
)

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
	if !strings.Contains(got, "IST (UTC+05:30, Asia/Kolkata); UTC ") {
		t.Errorf("current_date = %q, want IST with its offset, zone name, and the UTC instant", got)
	}
}
