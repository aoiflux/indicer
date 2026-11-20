package structs

// SearchReportSchemaVersion v2 (W7): counts are per object and per level,
// and the summary no longer adds the levels together; objects are named from
// their records; coverage says what was and was not searched.
const SearchReportSchemaVersion = "v2"

type SearchReport struct {
	SchemaVersion    string           `json:"schema_version,omitempty"`
	Query            string           `json:"query"`
	Terms            []string         `json:"terms,omitempty"`
	Operator         string           `json:"operator,omitempty"` // "and" or "or"
	Mode             string           `json:"mode,omitempty"`     // "scan" or "full-text"
	ExecutiveSummary string           `json:"executive_summary"`
	Summary          SearchSummary    `json:"summary"`
	Coverage         SearchCoverage   `json:"coverage"`
	Occurrences      []OccurrenceData `json:"occurances"`
}

// SearchSummary counts the objects holding a match, and their matches, per
// level. The same stored bytes are counted once in each level, so the levels
// are not added together.
type SearchSummary struct {
	Evidence     LevelSummary `json:"evidence"`
	Partitions   LevelSummary `json:"partitions"`
	IndexedFiles LevelSummary `json:"indexed_files"`
}

type LevelSummary struct {
	Objects     int   `json:"objects"`
	Occurrences int64 `json:"occurrences"`
}

// SearchCoverage says what a search read and what it did not.
type SearchCoverage struct {
	EvidenceSearched   int64    `json:"evidence_searched"`
	PartitionsSearched int64    `json:"partitions_searched"`
	FilesSearched      int64    `json:"files_searched"`
	BytesSearched      int64    `json:"bytes_searched"`
	NotStored          int64    `json:"evidence_not_stored,omitempty"`
	Quarantined        int64    `json:"evidence_index_quarantined,omitempty"`
	NotCandidates      int64    `json:"not_full_text_candidates,omitempty"`
	Unavailable        int64    `json:"files_without_content,omitempty"`
	Unreadable         int64    `json:"unreadable,omitempty"`
	Problems           []string `json:"problems,omitempty"`
}

type OccurrenceData struct {
	ArtefactHash string            `json:"artefact"`
	Level        string            `json:"level,omitempty"` // evidence, partition, indexed
	Path         string            `json:"path,omitempty"`
	Size         int64             `json:"size,omitempty"`
	Count        int               `json:"count"`
	TermCounts   map[string]int64  `json:"term_counts,omitempty"`
	BM25Score    float64           `json:"bm25_score,omitempty"`
	FileNames    []string          `json:"files,omitempty"`
	EvidenceFile *EvidenceFilePart `json:"evidence_file,omitempty"`
}

// Backward-compatible alias for older references.
type OccuranceData = OccurrenceData

type EvidenceFilePart struct {
	EvidenceFileHash  string         `json:"evidence_file_hash,omitempty"`
	EvidenceFileNames []string       `json:"evidence_file_names,omitempty"`
	Partition         *PartitionPart `json:"partition,omitempty"`
}

type PartitionPart struct {
	PartitionHash      string       `json:"partition_hash,omitempty"`
	PartitionPartNames []string     `json:"partition_part_names,omitempty"`
	Indexed            *IndexedPart `json:"indexed,omitempty"`
}

type IndexedPart struct {
	IndexedFileHash  string   `json:"indexed_file_hash,omitempty"`
	IndexedFileNames []string `json:"indexed_file_names,omitempty"`
}
