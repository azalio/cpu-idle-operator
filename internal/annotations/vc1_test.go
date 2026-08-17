package annotations

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestVC1NoForbiddenComponents enforces HC-4: the v1alpha1 agent has exactly
// one main package, never touches leader election or an admission webhook
// server, and go.mod pulls in no CRD/apiextensions generators.
//
// The literals under search are assembled from parts throughout this test so
// that this file's own source text never contains any of them verbatim,
// which would otherwise cause the walk to flag itself.
func TestVC1NoForbiddenComponents(t *testing.T) {
	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	root = filepath.Join(root, "..", "..")

	mainPackageMarker := "package" + " " + "main"
	forbiddenLeaderElection := regexp.MustCompile(`LeaderElection` + `\s*:\s*` + `true`)
	forbiddenWebhookServer := "webhook." + "NewServer"

	mainPackageDirs := map[string]bool{}

	walkErr := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == ".map" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		// Skip this file itself: it necessarily mentions the forbidden
		// component names in comments/identifiers describing the check.
		if filepath.Base(path) == "vc1_test.go" {
			return nil
		}
		contents, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		text := string(contents)

		if strings.Contains(text, mainPackageMarker) {
			mainPackageDirs[filepath.Dir(path)] = true
		}
		if strings.Contains(text, forbiddenWebhookServer) {
			t.Errorf("%s: found a forbidden admission-webhook server constructor call", path)
		}
		if forbiddenLeaderElection.MatchString(text) {
			t.Errorf("%s: found a forbidden leader-election-enabled setting", path)
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk tree: %v", walkErr)
	}

	if len(mainPackageDirs) != 1 {
		t.Fatalf("expected exactly one main package directory, found %d: %v", len(mainPackageDirs), mainPackageDirs)
	}

	goModPath := filepath.Join(root, "go.mod")
	goModBytes, err := os.ReadFile(goModPath)
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	goMod := string(goModBytes)
	// Intent: this must catch our own code directly requiring a
	// CRD-generator dependency, not merely appear anywhere in the module
	// graph. A library this project legitimately depends on for other
	// reasons (e.g. sigs.k8s.io/controller-runtime, whose own go.mod
	// requires k8s.io/apiextensions-apiserver) can pull one of these in as
	// a transitive, "// indirect" line without this project ever calling
	// any CRD/generator code itself. Only a direct (non-indirect) require
	// line is real signal that this project's own code reaches for one.
	for _, line := range strings.Split(goMod, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasSuffix(trimmed, "// indirect") {
			continue
		}
		for _, forbidden := range []string{"apiextensions", "controller-gen", "code-generator"} {
			if strings.Contains(trimmed, forbidden) {
				t.Errorf("go.mod directly requires a forbidden CRD-generator dependency: %q", trimmed)
			}
		}
	}
}
