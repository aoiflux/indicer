package detectors

import (
	"strings"

	"indicer/lib/microartefact/model"
)

type URLDetector struct {
	MaxResults int
}

func (d URLDetector) Name() string {
	return "url-pattern"
}

func (d URLDetector) Detect(_ model.FileRecord, content []byte) ([]model.Artefact, error) {
	maxResults := resolveMaxResults(d.MaxResults, defaultURLResults)
	matches := urlPattern.FindAllStringIndex(string(content), maxResults)
	results := make([]model.Artefact, 0, len(matches))
	for _, match := range matches {
		value := trimMatchPunctuation(string(content[match[0]:match[1]]))
		if len(value) < 10 {
			continue
		}
		end := match[0] + len(value)
		results = append(results, model.Artefact{
			Kind:       "url",
			Detector:   d.Name(),
			Value:      value,
			Summary:    truncate(value, 96),
			Confidence: 0.9,
			Span:       model.Span{Start: int64(match[0]), End: int64(end)},
		})
	}
	return results, nil
}

type KeyValueDetector struct {
	MaxResults int
}

func (d KeyValueDetector) Name() string {
	return "key-value"
}

func (d KeyValueDetector) Detect(_ model.FileRecord, content []byte) ([]model.Artefact, error) {
	maxResults := resolveMaxResults(d.MaxResults, defaultKeyValueResults)
	matches := keyValuePattern.FindAllStringSubmatchIndex(string(content), maxResults)
	results := make([]model.Artefact, 0, len(matches))
	for _, match := range matches {
		if len(match) < 6 {
			continue
		}
		key := strings.TrimSpace(string(content[match[2]:match[3]]))
		value := strings.TrimSpace(string(content[match[4]:match[5]]))
		if key == "http" || key == "https" || value == "" {
			continue
		}
		if len(value) > 256 {
			value = value[:256]
		}
		pair := key + "=" + value
		results = append(results, model.Artefact{
			Kind:       "key_value",
			Detector:   d.Name(),
			Value:      pair,
			Summary:    truncate(pair, 96),
			Confidence: 0.8,
			Span:       model.Span{Start: int64(match[0]), End: int64(match[1])},
			Attributes: map[string]string{"key": key, "value": value},
		})
	}
	return results, nil
}
