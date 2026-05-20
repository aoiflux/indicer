package detectors

import (
	"bytes"
	"fmt"

	"indicer/lib/microartefact/model"

	"www.velocidex.com/golang/regparser"
)

type RegistryHiveDetector struct {
	MaxResults int
}

func (d RegistryHiveDetector) Name() string {
	return "registry-hive"
}

func (d RegistryHiveDetector) Detect(_ model.FileRecord, content []byte) ([]model.Artefact, error) {
	registry, err := regparser.NewRegistry(bytes.NewReader(content))
	if err != nil {
		return nil, nil
	}

	root := registry.OpenKey("")
	if root == nil {
		return nil, nil
	}

	maxResults := resolveMaxResults(d.MaxResults, defaultRegistryValueResults)

	results := make([]model.Artefact, 0, maxResults)
	var walk func(key *regparser.CM_KEY_NODE, path string)
	walk = func(key *regparser.CM_KEY_NODE, path string) {
		if key == nil || len(results) >= maxResults {
			return
		}

		currentPath := joinRegistryPath(path, key.Name())
		lastWrite := fmt.Sprint(key.LastWriteTime())
		for _, value := range key.Values() {
			if len(results) >= maxResults {
				return
			}
			valueData := value.ValueData()
			valueName := value.ValueName()
			if valueName == "" {
				valueName = "(default)"
			}
			valueText := registryValueString(valueData)
			summary := currentPath + "\\" + valueName
			if valueText != "" {
				summary += "=" + truncate(valueText, 96)
			}
			results = append(results, model.Artefact{
				Kind:       "registry_value",
				Detector:   d.Name(),
				Value:      truncate(valueText, 256),
				Summary:    truncate(summary, 128),
				Confidence: 0.95,
				Attributes: map[string]string{
					"key_path":        currentPath,
					"value_name":      valueName,
					"value_type":      value.TypeString(),
					"last_write_time": lastWrite,
				},
			})
		}

		for _, child := range key.Subkeys() {
			walk(child, currentPath)
			if len(results) >= maxResults {
				return
			}
		}
	}

	walk(root, "")
	return results, nil
}
