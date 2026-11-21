package host

import (
	"os/exec"
	"strings"
	"testing"
)

// TestHostDoesNotLinkTheFrontend is the whole reason this package exists: a
// host that mounts the backend must not carry the 18 MiB capture. Checking the
// dependency list is exact, while a binary-size assertion would be a proxy.
func TestHostDoesNotLinkTheFrontend(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "./").CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v: %s", err, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasSuffix(strings.TrimSpace(line), "deepseek-harness-go/assets") {
			t.Fatal("package host links the frontend capture; a mounting host would embed it")
		}
	}
}
