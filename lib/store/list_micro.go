package store

import (
	"encoding/json"
	"fmt"

	"indicer/lib/microartefact"
)

func ListMicroArtefacts(repo *microartefact.GrapheneRepository) error {
	tree, err := repo.ReadHierarchy()
	if err != nil {
		return err
	}

	// Build output structure
	output := map[string]interface{}{
		"totalArtefacts":         tree.TotalArtefacts,
		"totalRelations":         tree.TotalRelations,
		"deterministicRelations": tree.DeterministicRelations,
		"probabilisticRelations": tree.ProbabilisticRelations,
		"evidenceFiles":          buildEvidenceFileData(tree.EvidenceFiles),
	}

	jsonData, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		return err
	}

	fmt.Println(string(jsonData))
	return nil
}

func buildEvidenceFileData(evidenceFiles []*microartefact.EvidenceFileNode) []map[string]interface{} {
	var evidenceFileList []map[string]interface{}

	for _, evidenceFile := range evidenceFiles {
		evidenceFileData := map[string]interface{}{
			"id":         evidenceFile.ID,
			"name":       evidenceFile.Name,
			"partitions": buildPartitionFileData(evidenceFile.Partitions),
		}
		evidenceFileList = append(evidenceFileList, evidenceFileData)
	}

	return evidenceFileList
}

func buildPartitionFileData(partitions []*microartefact.PartitionFileNode) []map[string]interface{} {
	var partitionList []map[string]interface{}

	for _, p := range partitions {
		pData := map[string]interface{}{
			"id":    p.ID,
			"name":  p.Name,
			"files": buildMicroIndexedFileData(p.Files),
		}
		partitionList = append(partitionList, pData)
	}

	return partitionList
}

func buildMicroIndexedFileData(files []*microartefact.IndexedFileArtefactView) []map[string]interface{} {
	var fileList []map[string]interface{}

	for _, f := range files {
		fData := map[string]interface{}{
			"id":            f.ID,
			"name":          f.Name,
			"path":          f.Path,
			"size":          f.Size,
			"artefacts":     buildMicroArtefactData(f.Artefacts),
			"artefactCount": len(f.Artefacts),
		}
		fileList = append(fileList, fData)
	}

	return fileList
}

func buildMicroArtefactData(artefacts []*microartefact.MicroArtefactNode) []map[string]interface{} {
	var artefactList []map[string]interface{}

	for _, a := range artefacts {
		aData := map[string]interface{}{
			"kind":       a.Kind,
			"detector":   a.Detector,
			"value":      a.Value,
			"summary":    a.Summary,
			"confidence": a.Confidence,
			"span": map[string]interface{}{
				"start": a.Start,
				"end":   a.End,
			},
		}
		artefactList = append(artefactList, aData)
	}

	return artefactList
}
