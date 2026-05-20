package detectors

import (
	"strings"

	"indicer/lib/microartefact/model"
)

type ScheduledTaskDetector struct{}

func (d ScheduledTaskDetector) Name() string {
	return "scheduled-task"
}

func (d ScheduledTaskDetector) Detect(_ model.FileRecord, content []byte) ([]model.Artefact, error) {
	text := string(content)
	if !scheduledTaskPattern.MatchString(text) {
		return nil, nil
	}
	return []model.Artefact{{
		Kind:       "scheduled_task",
		Detector:   d.Name(),
		Value:      "task artefact pattern matched",
		Summary:    "Scheduled task metadata candidate",
		Confidence: 0.7,
	}}, nil
}

type ServiceArtefactDetector struct{}

func (d ServiceArtefactDetector) Name() string {
	return "service-artefact"
}

func (d ServiceArtefactDetector) Detect(_ model.FileRecord, content []byte) ([]model.Artefact, error) {
	text := string(content)
	if !servicePattern.MatchString(text) {
		return nil, nil
	}
	return []model.Artefact{{
		Kind:       "service_artefact",
		Detector:   d.Name(),
		Value:      "service artefact pattern matched",
		Summary:    "Windows service metadata candidate",
		Confidence: 0.7,
	}}, nil
}

type BrowserArtefactDetector struct {
	MaxResults int
}

func (d BrowserArtefactDetector) Name() string {
	return "browser-artefact"
}

func (d BrowserArtefactDetector) Detect(_ model.FileRecord, content []byte) ([]model.Artefact, error) {
	maxResults := resolveMaxResults(d.MaxResults, defaultBrowserArtefactResult)
	text := string(content)
	if !browserPattern.MatchString(text) {
		return nil, nil
	}

	urlMatches := urlPattern.FindAllStringIndex(text, maxResults)
	results := make([]model.Artefact, 0, len(urlMatches)+1)
	for _, match := range urlMatches {
		value := strings.TrimSpace(text[match[0]:match[1]])
		results = append(results, model.Artefact{
			Kind:       "browser_url",
			Detector:   d.Name(),
			Value:      value,
			Summary:    truncate(value, 96),
			Confidence: 0.8,
			Span:       model.Span{Start: int64(match[0]), End: int64(match[1])},
		})
	}

	if len(results) == 0 {
		results = append(results, model.Artefact{
			Kind:       "browser_artefact",
			Detector:   d.Name(),
			Value:      "browser artefact pattern matched",
			Summary:    "Browser history/cookies/download candidate",
			Confidence: 0.65,
		})
	}

	return results, nil
}
