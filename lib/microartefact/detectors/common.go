package detectors

import (
	"encoding/hex"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"indicer/lib/microartefact/model"

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

const (
	defaultURLResults            = 64
	defaultKeyValueResults       = 64
	defaultIOCResults            = 128
	defaultRegistryValueResults  = 256
	defaultEVTXResults           = 128
	defaultLogLineResults        = 256
	defaultWindowsEventResults   = 64
	defaultBrowserArtefactResult = 64
)

type Detector interface {
	Name() string
	Detect(file model.FileRecord, content []byte) ([]model.Artefact, error)
}

// RawDetector is a detector that parses a file format itself, and so must be
// given the file's own bytes rather than its text.
type RawDetector interface {
	WantsRaw() bool
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

// filetimeUnixEpoch is 1970-01-01 as a Windows FILETIME.
const filetimeUnixEpoch = 116444736000000000

func windowsFiletimeToRFC3339(filetime uint64) string {
	// A FILETIME before 1970 used to wrap around in the unsigned subtraction
	// and print as a date tens of thousands of years ahead (GRA-18).
	if filetime <= filetimeUnixEpoch {
		return ""
	}
	unixSeconds := int64((filetime - filetimeUnixEpoch) / 10000000)
	if unixSeconds <= 0 {
		return ""
	}
	return time.Unix(unixSeconds, 0).UTC().Format(time.RFC3339)
}

func resolveMaxResults(configured, fallback int) int {
	if configured > 0 {
		return configured
	}
	return fallback
}

func trimMatchPunctuation(value string) string {
	return strings.TrimRight(value, ").,;]")
}

// truncate cuts value to at most limit bytes, at a rune boundary: cutting
// inside a rune left invalid UTF-8 in artefact values (GRA-18).
func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut]
}
