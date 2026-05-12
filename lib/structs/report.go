package structs

const SearchReportSchemaVersion = "v1"

type SearchReport struct {
	SchemaVersion    string           `json:"schema_version,omitempty"`
	Query            string           `json:"query"`
	ExecutiveSummary string           `json:"executive_summary"`
	Occurrences      []OccurrenceData `json:"occurances"`
}

type OccurrenceData struct {
	ArtefactHash string     `json:"artefact"`
	Count        int        `json:"count"`
	BM25Score    float64    `json:"bm25_score,omitempty"`
	FileNames    []string   `json:"files,omitempty"`
	Disk         *DiskImage `json:"disk,omitempty"`
}

// Backward-compatible alias for older references.
type OccuranceData = OccurrenceData

type DiskImage struct {
	DiskImageHash  string         `json:"disk_image_hash,omitempty"`
	DiskImageNames []string       `json:"disk_image_names,omitempty"`
	Partition      *PartitionPart `json:"partition,omitempty"`
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

func NewDiskImage() *DiskImage {
	return &DiskImage{
		DiskImageNames: []string{},
		Partition: &PartitionPart{
			PartitionPartNames: []string{},
			Indexed: &IndexedPart{
				IndexedFileNames: []string{},
			},
		},
	}
}
