package structs

type SearchReport struct {
	Query      string          `json:"query"`
	Occurances []OccuranceData `json:"occurances"`
}

type OccuranceData struct {
	ArtefactHash string     `json:"artefact"`
	Count        int        `json:"count"`
	FileNames    []string   `json:"files,omitempty"`
	Disk         *DiskImage `json:"disk,omitempty"`
}

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
