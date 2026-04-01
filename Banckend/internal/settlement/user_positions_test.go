package settlement

import "testing"

func TestRegistryFilterUnknownDeduplicatesWallets(t *testing.T) {
	registry := NewUserPositionRegistry()
	registry.MarkExists(42, "alice")

	unknown := registry.FilterUnknown(42, []string{"alice", "bob", "bob", "", "carol"})
	if len(unknown) != 2 {
		t.Fatalf("expected 2 unknown wallets, got %d", len(unknown))
	}
	if unknown[0] != "bob" || unknown[1] != "carol" {
		t.Fatalf("unexpected unknown wallets: %#v", unknown)
	}
}
