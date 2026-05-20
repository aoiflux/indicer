package microartefact

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"

	detectionpkg "indicer/lib/microartefact/detection"
	detectorpkg "indicer/lib/microartefact/detectors"
	parsingpkg "indicer/lib/microartefact/parsing"
	relationpkg "indicer/lib/microartefact/relation"
	reppkg "indicer/lib/microartefact/repository"
	specpkg "indicer/lib/microartefact/spec"
)

const defaultScanLimit = 4 << 20

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
	content = s.scanWindow(content)

	parsed, ok := s.parser.Parse(file, content)
	if !ok {
		return nil, nil, parsingpkg.Result{}, nil
	}

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
	artefacts, relations, parsed, err := s.extractWithParsed(file, content)
	if err != nil {
		return err
	}
	if len(artefacts) == 0 {
		return nil
	}
	applyParsedMetadata(&file, parsed)
	return s.repository.Store(file, artefacts, relations)
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
