package ui

import "indicer/internal/progress"

// runningLabels say what a step is doing while it runs: the live display
// reads "storing", and the step keeps its name once it ends.
var runningLabels = map[string]string{
	"compact":          "compacting",
	"deep":             "comparing in depth",
	"delete old index": "deleting old index",
	"entropy":          "measuring entropy",
	"full-text":        "extracting text",
	"graph":            "building graph",
	"hash file":        "hashing file",
	"identify":         "identifying",
	"index":            "indexing",
	"match":            "matching chunks",
	"micro":            "extracting artefacts",
	"partition hashes": "hashing partitions",
	"read query":       "reading query",
	"restore":          "restoring",
	"revrel":           "adding relations",
	"scan folder":      "scanning folder",
	"search":           "searching",
	"store":            "storing",
	"upgrade":          "upgrading",
	"verify":           "verifying",
}

// StepLabel is how a step reads in a progress display: what it does while
// it runs ("storing"), its name otherwise. A name it does not know reads as
// itself.
func StepLabel(name string, s progress.State) string {
	if s == progress.Running {
		if l, ok := runningLabels[name]; ok {
			return l
		}
	}
	return name
}
