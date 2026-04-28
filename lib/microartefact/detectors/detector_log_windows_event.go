package detectors

import (
	"strings"

	"indicer/lib/microartefact/model"
)

type LogLineDetector struct {
	MaxResults int
}

func (d LogLineDetector) Name() string {
	return "log-line"
}

func (d LogLineDetector) Detect(_ model.FileRecord, content []byte) ([]model.Artefact, error) {
	maxResults := resolveMaxResults(d.MaxResults, defaultLogLineResults)

	lines := strings.Split(string(content), "\n")
	results := make([]model.Artefact, 0, len(lines))
	offset := 0
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		lineLen := len(line)
		if trimmed == "" {
			offset += lineLen + 1
			continue
		}
		hasTimestamp := timestampPattern.MatchString(trimmed)
		hasSeverity := logLinePattern.MatchString(trimmed)
		if !hasTimestamp && !hasSeverity {
			offset += lineLen + 1
			continue
		}
		if len(results) >= maxResults {
			break
		}
		results = append(results, model.Artefact{
			Kind:       "log_line",
			Detector:   d.Name(),
			Value:      truncate(trimmed, 256),
			Summary:    truncate(trimmed, 96),
			Confidence: 0.75,
			Span:       model.Span{Start: int64(offset), End: int64(offset + lineLen)},
		})
		offset += lineLen + 1
	}

	return results, nil
}

type WindowsEventRecordDetector struct {
	MaxResults int
}

func (d WindowsEventRecordDetector) Name() string {
	return "windows-event-record"
}

func (d WindowsEventRecordDetector) Detect(_ model.FileRecord, content []byte) ([]model.Artefact, error) {
	maxResults := resolveMaxResults(d.MaxResults, defaultWindowsEventResults)
	text := string(content)
	matches := eventRecordPattern.FindAllStringIndex(text, maxResults)
	results := make([]model.Artefact, 0, len(matches))
	for _, match := range matches {
		value := strings.TrimSpace(text[match[0]:match[1]])
		results = append(results, model.Artefact{
			Kind:       "windows_event_record",
			Detector:   d.Name(),
			Value:      truncate(value, 256),
			Summary:    "XML Event record",
			Confidence: 0.7,
			Span:       model.Span{Start: int64(match[0]), End: int64(match[1])},
		})
	}
	return results, nil
}
