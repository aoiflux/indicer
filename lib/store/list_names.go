package store

import (
	"regexp"
	"sort"
	"strings"

	"indicer/lib/cnst"
)

func getMapKeys(m map[string]struct{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func trimHierName(value string) string {
	if strings.Contains(value, cnst.DataSeperator) {
		parts := strings.SplitN(value, cnst.DataSeperator, 3)
		if len(parts) >= 2 {
			return parts[len(parts)-1]
		}
	}
	return value
}

func firstCleanNameFromMap(values map[string]struct{}) string {
	if len(values) == 0 {
		return ""
	}
	keys := make([]string, 0, len(values))
	for name := range values {
		keys = append(keys, trimHierName(name))
	}
	sort.Strings(keys)
	return keys[0]
}

var partitionSuffixPattern = regexp.MustCompile(`_p[0-9]+$`)

func normalizeEvidenceFileName(value string) string {
	name := strings.TrimSpace(trimHierName(value))
	return partitionSuffixPattern.ReplaceAllString(name, "")
}
