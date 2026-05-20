package model

type ELFMetadata struct {
	Class                 int
	SectionCount          int
	RawSymbolCount        int
	NamedSymbolCount      int
	Sections              []ELFSectionMetadata
	SectionsTruncated     int
	NamedSymbols          []ELFSymbolMetadata
	NamedSymbolsTruncated int
}

type ELFSectionMetadata struct {
	Name string
	Type string
	Size uint64
}

type ELFSymbolMetadata struct {
	Name    string
	Value   uint64
	Size    uint64
	Version string
	Library string
}
