package parsing

import (
	fileparser "indicer/lib/microartefact/fileparser"
	"indicer/lib/microartefact/model"
)

type Kind = fileparser.Kind

const (
	KindText Kind = fileparser.KindText
	KindPDF  Kind = fileparser.KindPDF
	KindPE   Kind = fileparser.KindPE
	KindEVTX Kind = fileparser.KindEVTX
	KindELF  Kind = fileparser.KindELF
)

type Result struct {
	Kind    Kind
	Content []byte
	ELFMeta *model.ELFMetadata
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
	return Result{Kind: parsed.Kind, Content: parsed.Content, ELFMeta: parsed.ELFMeta}, true
}
