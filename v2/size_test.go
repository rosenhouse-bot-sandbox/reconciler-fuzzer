package v2_test

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const lineCap = 9000

func TestProductionCodeStaysUnderTheLineCap(t *testing.T) {
	lines := 0
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		data, err := os.ReadFile(path)
		lines += bytes.Count(data, []byte("\n"))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if lines == 0 || lines > lineCap {
		t.Errorf("v2 has %d lines of production Go, want between 1 and %d.", lines, lineCap)
	}
}
