package parsing

import (
	fileparser "indicer/lib/microartefact/fileparser"
	"indicer/lib/microartefact/model"
)

type Kind = fileparser.Kind

const (
	KindText     Kind = fileparser.KindText
	KindPDF      Kind = fileparser.KindPDF
	KindPE       Kind = fileparser.KindPE
	KindEVTX     Kind = fileparser.KindEVTX
	KindELF      Kind = fileparser.KindELF
	KindRegistry Kind = fileparser.KindRegistry
)

// Result is a file's content prepared for the detectors: Content for text
// detectors, Raw for those that parse the format, and Err when the format's
// parser failed.
type Result struct {
	Kind    Kind
	Content []byte
	Raw     []byte
	ELFMeta *model.ELFMetadata
	Err     error
}

type Parser interface {
	Parse(file model.FileRecord, content []byte) (Result, bool)
}

type DefaultParser struct{}

func (DefaultParser) Parse(file model.FileRecord, content []byte) (Result, bool) {
	parsed, ok := fileparser.Parse(file, content)
	if !ok {
		return Result{}, false
	}
	return Result{Kind: parsed.Kind, Content: parsed.Content, Raw: parsed.Raw, ELFMeta: parsed.ELFMeta, Err: parsed.Err}, true
}
