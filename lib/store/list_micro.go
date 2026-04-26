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
		"diskImages":             buildMicroDiskImageData(tree.DiskImages),
	}

	jsonData, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		return err
	}

	fmt.Println(string(jsonData))
	return nil
}

func buildMicroDiskImageData(diskImages []*microartefact.DiskImageNode) []map[string]interface{} {
	var diskImageList []map[string]interface{}

	for _, di := range diskImages {
		diData := map[string]interface{}{
			"id":         di.ID,
			"name":       di.Name,
			"partitions": buildMicroPartitionData(di.Partitions),
		}
		diskImageList = append(diskImageList, diData)
	}

	return diskImageList
}

func buildMicroPartitionData(partitions []*microartefact.PartitionNode) []map[string]interface{} {
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

func buildMicroIndexedFileData(files []*microartefact.IndexedFileNode) []map[string]interface{} {
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

func buildMicroArtefactData(artefacts []*microartefact.ArtefactNode) []map[string]interface{} {
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
