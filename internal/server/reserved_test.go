package server_test

import (
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/server"
)

func TestValidateExtensionNameAcceptsPlainNames(t *testing.T) {
	for _, name := range []string{"noop", "remarkable", "issue-tracker", "a1b2"} {
		if err := server.ValidateExtensionName(name); err != nil {
			t.Errorf("ValidateExtensionName(%q) = %v, want nil", name, err)
		}
	}
}

func TestValidateExtensionNameRejectsReserved(t *testing.T) {
	for _, name := range server.ReservedRouteNames {
		err := server.ValidateExtensionName(name)
		if err == nil {
			t.Errorf("ValidateExtensionName(%q) = nil, want an error (reserved)", name)
			continue
		}
		if !strings.Contains(err.Error(), name) {
			t.Errorf("ValidateExtensionName(%q) error = %v, want it to name the extension", name, err)
		}
	}
}

func TestValidateExtensionNameRejectsNonURLSafe(t *testing.T) {
	for _, name := range []string{"NoOp", "no_op", "no op", "-noop", "noop-", "noop--two", ""} {
		if err := server.ValidateExtensionName(name); err == nil {
			t.Errorf("ValidateExtensionName(%q) = nil, want an error (not URL-safe)", name)
		}
	}
}
