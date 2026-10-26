package store

import (
	"regexp"
	"strings"

	"indicer/lib/cnst"
)

func trimHierName(value string) string {
	if strings.Contains(value, cnst.DataSeperator) {
		parts := strings.SplitN(value, cnst.DataSeperator, 3)
		if len(parts) >= 2 {
			return parts[len(parts)-1]
		}
	}
	return value
}

var partitionSuffixPattern = regexp.MustCompile(`_p[0-9]+$`)

func normalizeEvidenceFileName(value string) string {
	name := strings.TrimSpace(trimHierName(value))
	return partitionSuffixPattern.ReplaceAllString(name, "")
}
