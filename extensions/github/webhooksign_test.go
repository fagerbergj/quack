package github

import "testing"

// Proves the QA mock sender's SignWebhookBody produces exactly what
// verifySignature (the real trust boundary) accepts.
func TestSignWebhookBodyRoundTrip(t *testing.T) {
	secret := []byte("qa-secret")
	body := []byte(`{"action":"labeled"}`)
	sig := SignWebhookBody(secret, body)
	if !verifySignature(secret, body, sig) {
		t.Fatalf("verifySignature rejected SignWebhookBody's own signature %q", sig)
	}
	if verifySignature(secret, []byte(`{"action":"other"}`), sig) {
		t.Fatal("verifySignature accepted a mismatched body")
	}
}
