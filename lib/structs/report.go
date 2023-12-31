package structs

type SearchReport struct {
	Query      string          `json:"query"`
	Occurances []OccuranceData `json:"occurances"`
}

type OccuranceData struct {
	ArtefactHash string   `json:"artefact"`
	Count        int      `json:"count"`
	FileNames    []string `json:"files"`
}
