package config

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

// ReadManifests decodes every document of the files, or of the YAML and
// JSON files in the directories, that paths name.
func ReadManifests(paths []string) ([]map[string]any, error) {
	var documents []map[string]any
	for _, path := range paths {
		files := []string{path}
		if info, err := os.Stat(path); err != nil {
			return nil, err
		} else if info.IsDir() {
			entries, err := os.ReadDir(path)
			if err != nil {
				return nil, err
			}
			files = nil
			for _, entry := range entries {
				if !entry.IsDir() && slices.Contains([]string{".yaml", ".yml", ".json"}, filepath.Ext(entry.Name())) {
					files = append(files, filepath.Join(path, entry.Name()))
				}
			}
		}
		for _, file := range files {
			read, err := readDocuments(file)
			if err != nil {
				return nil, err
			}
			documents = append(documents, read...)
		}
	}
	return documents, nil
}

func readDocuments(path string) ([]map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	reader := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
	var documents []map[string]any
	for {
		document, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return documents, nil
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		var decoded map[string]any
		if err := yaml.Unmarshal(document, &decoded); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if decoded != nil {
			documents = append(documents, decoded)
		}
	}
}
