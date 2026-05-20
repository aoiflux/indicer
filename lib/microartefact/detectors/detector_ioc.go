package detectors

import (
	"regexp"
	"strings"

	"indicer/lib/microartefact/model"
)

type IOCDetector struct {
	MaxResults int
}

func (d IOCDetector) Name() string {
	return "ioc"
}

func (d IOCDetector) Detect(_ model.FileRecord, content []byte) ([]model.Artefact, error) {
	maxResults := resolveMaxResults(d.MaxResults, defaultIOCResults)

	text := string(content)
	results := make([]model.Artefact, 0, maxResults)

	appendMatches := func(pattern *regexp.Regexp, kind string, confidence float32) {
		matches := pattern.FindAllStringIndex(text, maxResults)
		for _, match := range matches {
			if len(results) >= maxResults {
				return
			}
			value := strings.TrimSpace(text[match[0]:match[1]])
			results = append(results, model.Artefact{
				Kind:       kind,
				Detector:   d.Name(),
				Value:      value,
				Summary:    truncate(value, 96),
				Confidence: confidence,
				Span:       model.Span{Start: int64(match[0]), End: int64(match[1])},
			})
		}
	}

	appendMatches(urlPattern, "ioc_url", 0.9)
	appendMatches(ipv4Pattern, "ioc_ip", 0.88)
	appendMatches(emailPattern, "ioc_email", 0.88)
	appendMatches(domainPattern, "ioc_domain", 0.75)
	appendMatches(hashPattern, "ioc_hash", 0.92)

	return results, nil
}
