package store

import "strings"

func evidenceStatus(completed, failed bool) string {
	if completed {
		return "completed"
	}
	if failed {
		return "failed"
	}
	return "pending"
}

func normalizeStatusFilter(status string) string {
	s := strings.ToLower(strings.TrimSpace(status))
	switch s {
	case "completed", "pending", "failed":
		return s
	default:
		return "all"
	}
}

func matchesStatusFilter(filter, status string) bool {
	if filter == "all" {
		return true
	}
	return filter == status
}
