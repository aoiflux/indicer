package detectors

import (
	"bytes"
	"debug/pe"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"indicer/lib/microartefact/model"

	"www.velocidex.com/golang/evtx"
	"www.velocidex.com/golang/regparser"
)

var (
	urlPattern           = regexp.MustCompile(`(?i)\bhttps?://[^\s"'<>]+`)
	ipv4Pattern          = regexp.MustCompile(`\b(?:(?:25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)\.){3}(?:25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)\b`)
	emailPattern         = regexp.MustCompile(`(?i)\b[a-z0-9._%+\-]+@[a-z0-9.\-]+\.[a-z]{2,}\b`)
	domainPattern        = regexp.MustCompile(`(?i)\b(?:[a-z0-9](?:[a-z0-9\-]{0,61}[a-z0-9])?\.)+[a-z]{2,}\b`)
	hashPattern          = regexp.MustCompile(`\b(?:[A-Fa-f0-9]{32}|[A-Fa-f0-9]{40}|[A-Fa-f0-9]{64})\b`)
	keyValuePattern      = regexp.MustCompile(`(?m)([A-Za-z0-9._-]{2,64})\s*[:=]\s*([^\r\n]{1,256})`)
	logLinePattern       = regexp.MustCompile(`(?i)\b(?:error|warn|warning|info|debug|critical|fatal)\b`)
	timestampPattern     = regexp.MustCompile(`\b(?:\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}:\d{2}|[A-Z][a-z]{2}\s+\d{1,2}\s\d{2}:\d{2}:\d{2})\b`)
	eventRecordPattern   = regexp.MustCompile(`(?is)<Event\b.*?</Event>`)
	scheduledTaskPattern = regexp.MustCompile(`(?i)(?:<Task\b|schtasks\.exe|TaskName|<Triggers>|<Actions>)`)
	servicePattern       = regexp.MustCompile(`(?i)(?:\bsc\.exe\b|CreateService\(|SERVICE_AUTO_START|ServiceName|ImagePath)`)
	browserPattern       = regexp.MustCompile(`(?i)(?:History|Cookies|downloads?|places\.sqlite|Login Data|Visited Links)`)
)

type Detector interface {
	Name() string
	Detect(file model.FileRecord, content []byte) ([]model.Artefact, error)
}

type URLDetector struct {
	MaxResults int
}

func (d URLDetector) Name() string {
	return "url-pattern"
}

func (d URLDetector) Detect(_ model.FileRecord, content []byte) ([]model.Artefact, error) {
	maxResults := d.MaxResults
	if maxResults == 0 {
		maxResults = 64
	}

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
	maxResults := d.MaxResults
	if maxResults == 0 {
		maxResults = 64
	}

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

func Default() []Detector {
	return []Detector{
		URLDetector{},
		IOCDetector{},
		KeyValueDetector{},
		PEMetadataDetector{},
		RegistryHiveDetector{},
		EVTXDetector{},
		LogLineDetector{},
		WindowsEventRecordDetector{},
		ScheduledTaskDetector{},
		ServiceArtefactDetector{},
		BrowserArtefactDetector{},
	}
}

type IOCDetector struct {
	MaxResults int
}

func (d IOCDetector) Name() string {
	return "ioc"
}

func (d IOCDetector) Detect(_ model.FileRecord, content []byte) ([]model.Artefact, error) {
	maxResults := d.MaxResults
	if maxResults == 0 {
		maxResults = 128
	}

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

type PEMetadataDetector struct{}

func (d PEMetadataDetector) Name() string {
	return "pe-metadata"
}

func (d PEMetadataDetector) Detect(_ model.FileRecord, content []byte) ([]model.Artefact, error) {
	reader := bytes.NewReader(content)
	peFile, err := pe.NewFile(reader)
	if err != nil {
		return nil, nil
	}
	defer peFile.Close()

	results := make([]model.Artefact, 0, 32)

	var ts uint32
	switch header := peFile.FileHeader; {
	default:
		ts = header.TimeDateStamp
	}

	if ts != 0 {
		timeValue := time.Unix(int64(ts), 0).UTC().Format(time.RFC3339)
		results = append(results, model.Artefact{
			Kind:       "pe_timestamp",
			Detector:   d.Name(),
			Value:      timeValue,
			Summary:    "PE compile timestamp=" + timeValue,
			Confidence: 0.95,
		})
	}

	for _, section := range peFile.Sections {
		start := int64(section.Offset)
		end := start + int64(section.Size)
		if end > int64(len(content)) {
			end = int64(len(content))
		}
		entropy := estimateEntropy(content[start:end])
		value := fmt.Sprintf("%s size=%d entropy=%.2f", section.Name, section.Size, entropy)
		results = append(results, model.Artefact{
			Kind:       "pe_section",
			Detector:   d.Name(),
			Value:      value,
			Summary:    value,
			Confidence: 0.9,
			Span:       model.Span{Start: start, End: end},
			Attributes: map[string]string{
				"name":    section.Name,
				"size":    strconv.FormatUint(uint64(section.Size), 10),
				"entropy": fmt.Sprintf("%.4f", entropy),
			},
		})
	}

	imports, importErr := peFile.ImportedSymbols()
	if importErr == nil {
		for _, symbol := range imports {
			results = append(results, model.Artefact{
				Kind:       "pe_import",
				Detector:   d.Name(),
				Value:      symbol,
				Summary:    truncate(symbol, 96),
				Confidence: 0.9,
			})
		}
	}

	return results, nil
}

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

	maxResults := d.MaxResults
	if maxResults == 0 {
		maxResults = 256
	}

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

type EVTXDetector struct {
	MaxResults int
}

func (d EVTXDetector) Name() string {
	return "evtx-native"
}

func (d EVTXDetector) Detect(_ model.FileRecord, content []byte) ([]model.Artefact, error) {
	reader := bytes.NewReader(content)
	var header evtx.EVTXHeader
	if err := binary.Read(reader, binary.LittleEndian, &header); err != nil {
		return nil, nil
	}
	if string(header.Magic[:]) != evtx.EVTX_HEADER_MAGIC {
		return nil, nil
	}

	maxResults := d.MaxResults
	if maxResults == 0 {
		maxResults = 128
	}

	results := make([]model.Artefact, 0, maxResults)
	offset := int64(header.HeaderBlockSize)
	for offset < int64(len(content)) && len(results) < maxResults {
		chunk, err := evtx.NewChunk(bytes.NewReader(content), offset)
		if err != nil {
			break
		}
		if string(chunk.Header.Magic[:]) != evtx.EVTX_CHUNK_HEADER_MAGIC {
			break
		}

		records, err := chunk.Parse(0)
		if err != nil {
			break
		}
		for _, record := range records {
			if len(results) >= maxResults {
				break
			}
			eventText := fmt.Sprintf("%v", record.Event)
			if eventText == "<nil>" || eventText == "" {
				eventText = fmt.Sprintf("RecordID=%d", record.Header.RecordID)
			}
			results = append(results, model.Artefact{
				Kind:       "windows_event_record",
				Detector:   d.Name(),
				Value:      truncate(eventText, 512),
				Summary:    truncate(eventText, 128),
				Confidence: 0.95,
				Attributes: map[string]string{
					"record_id":    strconv.FormatUint(record.Header.RecordID, 10),
					"timestamp":    windowsFiletimeToRFC3339(record.Header.FileTime),
					"chunk_offset": strconv.FormatInt(offset, 10),
				},
			})
		}

		offset += evtx.EVTX_CHUNK_SIZE
	}

	return results, nil
}

type LogLineDetector struct {
	MaxResults int
}

func (d LogLineDetector) Name() string {
	return "log-line"
}

func (d LogLineDetector) Detect(_ model.FileRecord, content []byte) ([]model.Artefact, error) {
	maxResults := d.MaxResults
	if maxResults == 0 {
		maxResults = 256
	}

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
	maxResults := d.MaxResults
	if maxResults == 0 {
		maxResults = 64
	}
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
	maxResults := d.MaxResults
	if maxResults == 0 {
		maxResults = 64
	}

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

func estimateEntropy(data []byte) float64 {
	if len(data) == 0 {
		return 0
	}
	var freq [256]int
	for _, b := range data {
		freq[b]++
	}
	length := float64(len(data))
	entropy := 0.0
	for _, count := range freq {
		if count == 0 {
			continue
		}
		probability := float64(count) / length
		entropy -= probability * log2(probability)
	}
	return entropy
}

func log2(value float64) float64 {
	return math.Log(value) / math.Log(2)
}

func joinRegistryPath(base, name string) string {
	if name == "" {
		return base
	}
	if base == "" {
		return name
	}
	return base + "\\" + name
}

func registryValueString(valueData *regparser.ValueData) string {
	if valueData == nil {
		return ""
	}
	if valueData.Error != nil {
		return valueData.Error.Error()
	}
	if valueData.String != "" {
		return valueData.String
	}
	if len(valueData.MultiSz) > 0 {
		return strings.Join(valueData.MultiSz, ";")
	}
	if valueData.Uint64 != 0 {
		return strconv.FormatUint(valueData.Uint64, 10)
	}
	if len(valueData.Data) > 0 {
		return hex.EncodeToString(valueData.Data)
	}
	return ""
}

func windowsFiletimeToRFC3339(filetime uint64) string {
	if filetime == 0 {
		return ""
	}
	unixSeconds := int64((filetime - 116444736000000000) / 10000000)
	if unixSeconds <= 0 {
		return ""
	}
	return time.Unix(unixSeconds, 0).UTC().Format(time.RFC3339)
}

func appendStringArtefact(results []model.Artefact, content []byte, start, end, minLength, maxLength, maxResults int, detector string) []model.Artefact {
	if len(results) >= maxResults {
		return results
	}
	segment := strings.TrimSpace(string(content[start:end]))
	if len(segment) < minLength || !hasSignal(segment) {
		return results
	}
	if len(segment) > maxLength {
		segment = segment[:maxLength]
		end = start + maxLength
	}
	return append(results, model.Artefact{
		Kind:       "ascii_string",
		Detector:   detector,
		Value:      segment,
		Summary:    truncate(segment, 96),
		Confidence: 0.35,
		Span:       model.Span{Start: int64(start), End: int64(end)},
	})
}

func isPrintableASCII(b byte) bool {
	return b == '\t' || (b >= 32 && b <= 126)
}

func hasSignal(value string) bool {
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

func trimMatchPunctuation(value string) string {
	return strings.TrimRight(value, ").,;]")
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
