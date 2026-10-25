package modelconfig

import (
	"os"
	"testing"
)

// TestLocalPatch is a diagnostic for the developer machine. It is skipped
// unless DSH_GO_LOCAL_PATCH points at a patch file.
func TestLocalPatch(t *testing.T) {
	path := os.Getenv("DSH_GO_LOCAL_PATCH")
	if path == "" {
		t.Skip("DSH_GO_LOCAL_PATCH not set")
	}
	catalog, err := LoadPatch(path)
	if err != nil {
		t.Fatalf("load %s: %v", path, err)
	}
	if catalog == nil {
		t.Fatalf("no catalog in %s", path)
	}
	t.Logf("default=%+v routes=%d failures=%+v", catalog.Default, catalog.RouteCount(), catalog.Failures)
	for _, provider := range catalog.Providers {
		t.Logf("provider %s name=%q protocol=%s base=%s models=%d", provider.ID, provider.Name, provider.Protocol, provider.BaseURL, len(provider.Models))
		for _, model := range provider.Models {
			t.Logf("  %s %q efforts=%v default=%q", model.ID, model.Name, modelIDs(model.Efforts), model.DefaultEffort)
		}
	}
}
