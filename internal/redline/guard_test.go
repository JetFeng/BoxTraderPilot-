// Red-line guard (§4.1, §8.2): this repository must never contain
// order-trading identifiers. The forbidden words are assembled at
// runtime so this file itself stays clean for the very grep it guards.
package redline

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// forbiddenFragments lists word pieces; each guard word is a
// concatenation of fragments, e.g. fragments[0][0]+fragments[0][1].
var forbiddenFragments = [][2]string{
	{"Place", "Order"},
	{"Replace", "Order"},
	{"Cancel", "Order"},
	{"Sub", "mit"},
}

func TestNoTradingIdentifiers(t *testing.T) {
	root := "../.." // module root relative to this package
	var forbidden []string
	for _, f := range forbiddenFragments {
		forbidden = append(forbidden, strings.ToLower(f[0]+f[1]))
	}

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "testdata", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		lower := strings.ToLower(string(raw))
		for _, word := range forbidden {
			if strings.Contains(lower, word) {
				t.Errorf("%s contains forbidden identifier %q", path, word)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
