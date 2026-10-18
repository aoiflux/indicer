package microartefact

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"unicode/utf8"

	"indicer/internal/safe"
	detectionpkg "indicer/lib/microartefact/detection"
	detectorpkg "indicer/lib/microartefact/detectors"
	parsingpkg "indicer/lib/microartefact/parsing"
	relationpkg "indicer/lib/microartefact/relation"
	reppkg "indicer/lib/microartefact/repository"
	specpkg "indicer/lib/microartefact/spec"
)

const defaultScanLimit = 4 << 20

// RawLimit is how much of a file the structured parsers (PE, ELF, PDF, EVTX,
// registry) read. Content used to be cut to 4 MiB before parsing, so any
// larger binary or PDF failed to parse and looked like a file with no
// artefacts (GRA-16). A larger file is scanned as text from its start, and
// recorded as too large.
const RawLimit = 64 << 20

// Outcome is what extraction found in one file.
type Outcome struct {
	Status    string
	Kind      string
	Artefacts int
	Relations int
	// ParseErr is why the file's format parser failed (StatusParseFailed).
	ParseErr error
}

type Service struct {
	repository      reppkg.Repository
	parser          parsingpkg.Parser
	detectionEngine *detectionpkg.Engine
	relationEngine  *relationpkg.Engine
	spec            specpkg.Sheet
	maxScanBytes    int
}

func NewService(repository reppkg.Repository, detectors ...detectorpkg.Detector) *Service {
	return NewServiceWithSpec(repository, specpkg.Default(), detectors...)
}

func NewServiceWithSpec(repository reppkg.Repository, sheet specpkg.Sheet, detectors ...detectorpkg.Detector) *Service {
	if repository == nil {
		repository = reppkg.NopRepository{}
	}
	if len(detectors) == 0 {
		detectors = detectorpkg.Default()
	}

	return &Service{
		repository:      repository,
		parser:          parsingpkg.DefaultParser{},
		detectionEngine: detectionpkg.NewEngine(detectors, sheet.DetectionMethods()),
		relationEngine:  relationpkg.NewEngine(relationpkg.DefaultBuilders(), sheet.RelationMethods()),
		spec:            sheet,
		maxScanBytes:    defaultScanLimit,
	}
}

func (s *Service) Spec() specpkg.Sheet {
	return s.spec
}

func (s *Service) Close() error {
	return s.repository.Close()
}

func (s *Service) Extract(file FileRecord, content []byte) ([]Artefact, error) {
	artefacts, _, err := s.ExtractWithRelations(file, content)
	return artefacts, err
}

func (s *Service) ExtractWithRelations(file FileRecord, content []byte) ([]Artefact, []Relation, error) {
	artefacts, relations, _, err := s.extractWithParsed(file, content)
	return artefacts, relations, err
}

func (s *Service) extractWithParsed(file FileRecord, content []byte) ([]Artefact, []Relation, parsingpkg.Result, error) {
	// The PE, ELF, PDF and EVTX parsers read evidence content: a panic fails
	// this file, not the process (PAR-1).
	type parseResult struct {
		r  parsingpkg.Result
		ok bool
	}
	pr, err := safe.Value(safe.Site{Component: "microartefact/parse", Op: "parse", Path: file.Path}, func() (parseResult, error) {
		r, ok := s.parser.Parse(file, content)
		return parseResult{r, ok}, nil
	})
	if err != nil {
		return nil, nil, parsingpkg.Result{}, err
	}
	parsed, ok := pr.r, pr.ok
	if !ok {
		return nil, nil, parsingpkg.Result{}, nil
	}
	// Text detectors scan the start of the text; detectors that parse the
	// format get all of its bytes.
	parsed.Content = s.scanWindow(parsed.Content)

	artefacts, err := s.detectionEngine.Detect(file, parsed)
	if err != nil {
		return nil, nil, parsingpkg.Result{}, err
	}

	artefacts = dedupeArtefacts(artefacts)
	sortArtefactsByOffset(artefacts)

	relations := s.relationEngine.Relate(file, artefacts)
	return artefacts, relations, parsed, nil
}

func (s *Service) Process(file FileRecord, content []byte) error {
	_, err := s.ProcessFile(file, content, int64(len(content)), false)
	return err
}

// ProcessFile extracts the artefacts of one file and stores them. content is
// the file's bytes, at most RawLimit of them; size is the file's size. With
// replace, the artefacts an earlier run stored for the file are replaced.
//
// A repository that records results (the graph) records every file it is
// given, artefacts or not, so a later run knows the file was examined.
func (s *Service) ProcessFile(file FileRecord, content []byte, size int64, replace bool) (Outcome, error) {
	var (
		artefacts []Artefact
		relations []Relation
		parsed    parsingpkg.Result
		out       Outcome
		err       error
	)
	if size > RawLimit || int64(len(content)) > RawLimit {
		// Too large to parse its structure: its start is scanned as text.
		window := s.scanWindow(content)
		if !looksLikeTextWindow(window) {
			window = extractPrintable(window)
		}
		artefacts, relations, parsed, err = s.extractWithParsed(FileRecord{
			Hash: file.Hash, Name: file.Name, Path: file.Path, Size: file.Size, FileType: "txt",
		}, window)
		out.Status = reppkg.StatusTooLarge
	} else {
		artefacts, relations, parsed, err = s.extractWithParsed(file, content)
	}
	if err != nil {
		return out, err
	}
	out.Kind = string(parsed.Kind)
	out.Artefacts, out.Relations = len(artefacts), len(relations)
	switch {
	case out.Status != "":
	case parsed.Kind == "":
		out.Status = reppkg.StatusUnsupported
	case parsed.Err != nil:
		out.Status, out.ParseErr = reppkg.StatusParseFailed, parsed.Err
	case len(artefacts) == 0:
		out.Status = reppkg.StatusNone
	default:
		out.Status = reppkg.StatusExtracted
	}

	applyParsedMetadata(&file, parsed)
	if recorder, ok := s.repository.(reppkg.ResultRepository); ok {
		return out, recorder.Record(file, artefacts, relations, reppkg.Result{Status: out.Status, Replace: replace})
	}
	if len(artefacts) == 0 {
		return out, nil
	}
	return out, s.repository.Store(file, artefacts, relations)
}

func applyParsedMetadata(file *FileRecord, parsed parsingpkg.Result) {
	if file == nil {
		return
	}
	if parsed.ELFMeta != nil {
		file.ELFMeta = parsed.ELFMeta
	}
}

func (s *Service) scanWindow(content []byte) []byte {
	if len(content) <= s.maxScanBytes {
		return content
	}
	return content[:s.maxScanBytes]
}

func dedupeArtefacts(artefacts []Artefact) []Artefact {
	seen := make(map[string]struct{}, len(artefacts))
	unique := make([]Artefact, 0, len(artefacts))
	for _, artefact := range artefacts {
		key := artefactFingerprint(artefact)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		unique = append(unique, artefact)
	}
	return unique
}

func sortArtefactsByOffset(artefacts []Artefact) {
	sort.Slice(artefacts, func(left, right int) bool {
		if artefacts[left].Span.Start == artefacts[right].Span.Start {
			return artefacts[left].Kind < artefacts[right].Kind
		}
		return artefacts[left].Span.Start < artefacts[right].Span.Start
	})
}

func artefactFingerprint(artefact Artefact) string {
	return hashText(
		artefact.Kind,
		artefact.Detector,
		strconv.FormatInt(artefact.Span.Start, 10),
		strconv.FormatInt(artefact.Span.End, 10),
		artefact.Value,
	)
}

func hashText(value string, rest ...string) string {
	hasher := sha256.New()
	_, _ = hasher.Write([]byte(value))
	for _, next := range rest {
		_, _ = hasher.Write([]byte{0})
		_, _ = hasher.Write([]byte(next))
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

// looksLikeTextWindow reports whether a window of content reads as text.
func looksLikeTextWindow(window []byte) bool {
	sample := window[:min(len(window), 4096)]
	if len(sample) == 0 || !utf8.Valid(sample) {
		return false
	}
	printable := 0
	for _, b := range sample {
		if b == '\n' || b == '\r' || b == '\t' || (b >= 32 && b < 127) {
			printable++
		}
	}
	return printable*100 >= len(sample)*85
}

// extractPrintable keeps the runs of printable ASCII of at least 6 bytes,
// one per line.
func extractPrintable(content []byte) []byte {
	const minLen = 6
	out := make([]byte, 0, len(content)/8)
	start := -1
	for i := 0; i <= len(content); i++ {
		printable := i < len(content) && (content[i] == '\t' || (content[i] >= 32 && content[i] < 127))
		switch {
		case printable && start < 0:
			start = i
		case !printable && start >= 0:
			if i-start >= minLen {
				out = append(out, content[start:i]...)
				out = append(out, '\n')
			}
			start = -1
		}
	}
	return out
}
