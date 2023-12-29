package structs

type SearchReport struct {
	Query     string        `json:"query"`
	Occurance OccuranceData `json:"occurance_data"`
}

type OccuranceData struct {
	ArtefactHash string   `json:"artefact_hash"`
	Count        int      `json:"count"`
	FileNames    []string `json:"file_names"`
}
