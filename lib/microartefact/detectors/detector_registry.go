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

// WantsRaw: the detector parses the hive itself.
func (d RegistryHiveDetector) WantsRaw() bool { return true }

// maxRegistryDepth bounds the key walk. A hive is evidence, and a key can
// name an ancestor as its subkey; the walk was recursive and unbounded, so
// such a hive never ended or overflowed the stack (GRA-13).
const maxRegistryDepth = 512

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

	results := make([]model.Artefact, 0, min(maxResults, 64))
	type frame struct {
		key   *regparser.CM_KEY_NODE
		path  string
		depth int
	}
	visited := map[int64]struct{}{}
	stack := []frame{{key: root}}
	for len(stack) > 0 && len(results) < maxResults {
		f := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		key := f.key
		if key == nil || f.depth > maxRegistryDepth {
			continue
		}
		if _, seen := visited[key.Offset]; seen {
			continue
		}
		visited[key.Offset] = struct{}{}

		currentPath := joinRegistryPath(f.path, key.Name())
		lastWrite := fmt.Sprint(key.LastWriteTime())
		for _, value := range key.Values() {
			if len(results) >= maxResults {
				break
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

		// Pushed in reverse, so subkeys are visited in their stored order.
		children := key.Subkeys()
		for i := len(children) - 1; i >= 0; i-- {
			stack = append(stack, frame{key: children[i], path: currentPath, depth: f.depth + 1})
		}
	}
	return results, nil
}
