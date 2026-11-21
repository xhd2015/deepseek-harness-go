package dshsource

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReadyReportsWhatIsMissing keeps the "not built" message honest: the
// caller must be able to say which artifact is absent before spending minutes
// on a build.
func TestReadyReportsWhatIsMissing(t *testing.T) {
	root := t.TempDir()
	if cli, dist := Ready(root); cli || dist {
		t.Fatalf("empty checkout reports cli=%v dist=%v", cli, dist)
	}
	for _, rel := range []string{cliEntry, webDistIndex} {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	if cli, dist := Ready(root); !cli || !dist {
		t.Fatalf("built checkout reports cli=%v dist=%v", cli, dist)
	}
}

func TestServeRefusesAnUnbuiltCheckout(t *testing.T) {
	root := t.TempDir()
	_, err := Serve(context.Background(), ServeOptions{Root: root})
	if err == nil {
		t.Fatal("an unbuilt checkout was served")
	}
	if !strings.Contains(err.Error(), cliEntry) {
		t.Fatalf("err = %v, want it to name %s", err, cliEntry)
	}
}

func TestBuildRefusesANonCheckout(t *testing.T) {
	_, err := Build(context.Background(), BuildOptions{Root: t.TempDir()})
	if err == nil {
		t.Fatal("a directory without the Web package was built")
	}
	if !strings.Contains(err.Error(), webPackage) {
		t.Fatalf("err = %v, want it to name %s", err, webPackage)
	}
}

// TestScrubbedEnvDropsAmbientFacts proves the child server never inherits this
// process's DSH_* runtime facts, so the scratch home is really scratch.
func TestScrubbedEnvDropsAmbientFacts(t *testing.T) {
	got := scrubbedEnv([]string{"PATH=/bin", "DSH_HOME=/real", "DSH_TOKEN=t", "HOME=/root"})
	joined := strings.Join(got, ",")
	if strings.Contains(joined, "DSH_") {
		t.Fatalf("env = %v", got)
	}
	if !strings.Contains(joined, "PATH=/bin") || !strings.Contains(joined, "HOME=/root") {
		t.Fatalf("env = %v, want the unrelated entries kept", got)
	}
}
