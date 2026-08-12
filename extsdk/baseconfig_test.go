package sdk

import "testing"

func TestBaseConfigDefaults(t *testing.T) {
	var bc BaseConfig
	// Default is not set, so Enabled should be nil (not false).
	if bc.Enabled != nil {
		t.Error("BaseConfig.Enabled should be nil by default")
	}
}
