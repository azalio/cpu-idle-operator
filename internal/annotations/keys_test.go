package annotations

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestVC2SingleAnnotationSource walks the module tree and fails if the
// annotation domain literal appears in more than one .go file. The literal
// itself is assembled from parts here so this test file's own source text
// never contains it verbatim, which would otherwise defeat the check.
func TestVC2SingleAnnotationSource(t *testing.T) {
	literal := "cpu." + "azalio" + ".net/"

	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	// Walk from the module root (two levels up from internal/annotations).
	root = filepath.Join(root, "..", "..")

	var matches []string
	walkErr := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		contents, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if strings.Contains(string(contents), literal) {
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				rel = path
			}
			matches = append(matches, rel)
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk tree: %v", walkErr)
	}

	if len(matches) != 1 {
		t.Fatalf("expected literal %q in exactly one file, found in %v", literal, matches)
	}
	want := filepath.Join("internal", "annotations", "keys.go")
	if matches[0] != want {
		t.Fatalf("expected sole occurrence in %s, found in %s", want, matches[0])
	}
}
