package enrichment

import (
	"indicer/lib/util"
	"sort"
	"strconv"
	"strings"

	graphenedb "github.com/aoiflux/graphene"
	graphstore "github.com/aoiflux/graphene/store"
	"github.com/vmihailenco/msgpack/v5"
)

// GrapheneRepository is a graphdb adapter backed by the existing graphene store.
type GrapheneRepository struct {
	graph *graphenedb.Graph
}

const storeDirName = "graph"

var (
	nodeTypeEvidenceFile = graphstore.CustomNodeType(0)
	nodeTypePartition    = graphstore.CustomNodeType(1)
	nodeTypeIndexedFile  = graphstore.CustomNodeType(2)
	nodeTypeFile         = graphstore.CustomNodeType(3)
)

func OpenGrapheneRepository(root string) (*GrapheneRepository, error) {
	graph, err := graphenedb.Open(util.GraphPath(root))
	if err != nil {
		return nil, err
	}
	return &GrapheneRepository{graph: graph}, nil
}

func (repository *GrapheneRepository) UpsertEvidence(record EvidenceRecord) error {
	if repository == nil || repository.graph == nil {
		return nil
	}

	evidenceNode, err := repository.upsertNode(
		"evidence_file_id",
		record.HashBase64,
		[]graphstore.NodeType{nodeTypeEvidenceFile},
		map[string]any{
			"evidence_file_id": record.HashBase64,
			"name":             record.Name,
			"path":             record.Path,
			"size":             record.Size,
			"evidence_hash":    record.HashBase64,
			"node_type":        "evidence",
		},
	)
	if err != nil {
		return err
	}

	if err := repository.graph.IndexNodeProperties(evidenceNode, map[string][]byte{
		"evidence_hash": []byte(record.HashBase64),
		"name":          []byte(record.Name),
		"path":          []byte(record.Path),
	}); err != nil {
		return err
	}

	return nil
}

func (repository *GrapheneRepository) UpsertPartition(record PartitionRecord) error {
	if repository == nil || repository.graph == nil {
		return nil
	}

	evidenceNode, err := repository.upsertNode(
		"evidence_file_id",
		record.EvidenceFileID,
		[]graphstore.NodeType{nodeTypeEvidenceFile},
		map[string]any{"evidence_file_id": record.EvidenceFileID, "name": record.EvidenceFileName},
	)
	if err != nil {
		return err
	}

	partitionNode, err := repository.upsertNode(
		"partition_id",
		record.PartitionID,
		[]graphstore.NodeType{nodeTypePartition},
		map[string]any{
			"partition_id":     record.PartitionID,
			"name":             record.PartitionName,
			"evidence_file_id": record.EvidenceFileID,
		},
	)
	if err != nil {
		return err
	}

	return repository.ensureContains(evidenceNode, partitionNode)
}

func (repository *GrapheneRepository) UpsertFile(record FileRecord) error {
	if repository == nil || repository.graph == nil {
		return nil
	}

	// Always create/get evidence file node
	evidenceNode, err := repository.upsertNode(
		"evidence_file_id",
		record.EvidenceFileID,
		[]graphstore.NodeType{nodeTypeEvidenceFile},
		map[string]any{"evidence_file_id": record.EvidenceFileID, "name": record.EvidenceFileName},
	)
	if err != nil {
		return err
	}

	var parentNode graphstore.NodeID = evidenceNode

	// For partition and indexed_file levels, create/get partition node
	if record.Level != "evidence_file" && record.PartitionID != "" {
		partitionNode, err := repository.upsertNode(
			"partition_id",
			record.PartitionID,
			[]graphstore.NodeType{nodeTypePartition},
			map[string]any{"partition_id": record.PartitionID, "name": record.PartitionName, "evidence_file_id": record.EvidenceFileID},
		)
		if err != nil {
			return err
		}
		if err := repository.ensureContains(evidenceNode, partitionNode); err != nil {
			return err
		}
		parentNode = partitionNode

		// For indexed_file level, create/get indexed file node
		if record.Level == "indexed_file" && record.IndexedFileID != "" {
			indexedProps := map[string]any{
				"indexed_file_id":  record.IndexedFileID,
				"hash":             record.IndexedFileHash,
				"size":             record.Size,
				"partition_id":     record.PartitionID,
				"evidence_file_id": record.EvidenceFileID,
				"file_type":        record.FileType,
				"has_entropy":      record.HasEntropy,
			}
			if record.HasEntropy {
				indexedProps["entropy"] = record.Entropy
			}
			indexedNode, err := repository.upsertNode(
				"indexed_file_id",
				record.IndexedFileID,
				[]graphstore.NodeType{nodeTypeIndexedFile},
				indexedProps,
			)
			if err != nil {
				return err
			}
			if err := repository.ensureContains(partitionNode, indexedNode); err != nil {
				return err
			}
			parentNode = indexedNode
		}
	}

	// Create FILE node for this specific file
	fileNodeID := record.buildFileNodeID()
	fileHash := ""
	switch record.Level {
	case "evidence_file":
		fileHash = record.EvidenceFileID
	case "partition":
		fileHash = record.PartitionID
	case "indexed_file":
		fileHash = record.IndexedFileHash
	}
	fileProps := map[string]any{
		"file_node_id":     fileNodeID,
		"name":             record.FileName,
		"path":             record.Path,
		"hash":             fileHash,
		"size":             record.Size,
		"is_deleted":       record.IsDeleted,
		"is_fragmented":    record.IsFragmented,
		"level":            record.Level,
		"file_type":        record.FileType,
		"mime_type":        record.MimeType,
		"tags":             record.Tags,
		"has_entropy":      record.HasEntropy,
		"evidence_file_id": record.EvidenceFileID,
		"partition_id":     record.PartitionID,
		"indexed_file_id":  record.IndexedFileID,
	}
	if record.HasEntropy {
		fileProps["entropy"] = record.Entropy
	}
	fileNode, err := repository.upsertNode(
		"file_node_id",
		fileNodeID,
		[]graphstore.NodeType{nodeTypeFile},
		fileProps,
	)
	if err != nil {
		return err
	}

	if err := repository.graph.IndexNodeProperties(fileNode, map[string][]byte{
		"name":          []byte(record.FileName),
		"level":         []byte(record.Level),
		"is_deleted":    []byte(strconv.FormatBool(record.IsDeleted)),
		"is_fragmented": []byte(strconv.FormatBool(record.IsFragmented)),
		"mime_type":     []byte(record.MimeType),
		"tags":          []byte(joinTags(record.Tags)),
	}); err != nil {
		return err
	}

	// Create edge from parent to file
	if err := repository.ensureContains(parentNode, fileNode); err != nil {
		return err
	}

	return nil
}

func (repository *GrapheneRepository) Close() error {
	if repository == nil || repository.graph == nil {
		return nil
	}
	return repository.graph.Close()
}

func (repository *GrapheneRepository) upsertNode(indexKey, indexValue string, labels []graphstore.NodeType, properties map[string]any) (graphstore.NodeID, error) {
	hits, err := repository.graph.NodesByProperty(indexKey, []byte(indexValue))
	if err != nil {
		return 0, err
	}
	if len(hits) > 0 {
		return hits[0], nil
	}

	id, err := repository.graph.AddNode(&graphstore.Node{Labels: labels, Properties: mustPack(properties)})
	if err != nil {
		return 0, err
	}
	if err := repository.graph.IndexNodeProperty(id, indexKey, []byte(indexValue)); err != nil {
		return 0, err
	}
	return id, nil
}

func (repository *GrapheneRepository) ensureContains(src, dst graphstore.NodeID) error {
	exists, err := repository.graph.EdgeExists(src, dst, []graphstore.EdgeType{graphstore.EdgeTypeContains})
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	_, err = repository.graph.AddEdge(&graphstore.Edge{Src: src, Dst: dst, Labels: []graphstore.EdgeType{graphstore.EdgeTypeContains}})
	return err
}

func mustPack(value any) []byte {
	packed, err := msgpack.Marshal(value)
	if err != nil {
		panic(err)
	}
	return packed
}

// ReadHierarchy walks the enrichment graphdb and returns the full
// evidence_file → partition → indexed_file hierarchy with file-level metadata.
func (repository *GrapheneRepository) ReadHierarchy() (*EnrichmentHierarchy, error) {
	if repository == nil || repository.graph == nil {
		return &EnrichmentHierarchy{}, nil
	}

	tree := &EnrichmentHierarchy{}
	evidenceFileIDs, err := repository.graph.NodesByType(nodeTypeEvidenceFile)
	if err != nil {
		return nil, err
	}

	contEdges := []graphstore.EdgeType{graphstore.EdgeTypeContains}

	for _, evidenceID := range evidenceFileIDs {
		evidenceNode, err := repository.graph.GetNode(evidenceID)
		if err != nil {
			return nil, err
		}
		dProps := unpackProps(evidenceNode.Properties)
		evidenceFile := &EnrichmentEvidenceFileNode{
			ID:   strProp(dProps, "evidence_file_id"),
			Name: strProp(dProps, "name"),
		}

		partNeighbours, err := repository.graph.Neighbours(evidenceID, graphstore.DirectionOutbound, contEdges)
		if err != nil {
			return nil, err
		}

		for _, pn := range partNeighbours {
			if !pn.Node.HasLabel(nodeTypePartition) {
				continue
			}
			pProps := unpackProps(pn.Node.Properties)
			partition := &EnrichmentPartitionNode{
				ID:   strProp(pProps, "partition_id"),
				Name: strProp(pProps, "name"),
			}

			// Get indexed file nodes
			indexedNeighbours, err := repository.graph.Neighbours(pn.Node.ID, graphstore.DirectionOutbound, contEdges)
			if err != nil {
				return nil, err
			}

			for _, fn := range indexedNeighbours {
				if !fn.Node.HasLabel(nodeTypeIndexedFile) {
					continue
				}
				fProps := unpackProps(fn.Node.Properties)
				indexedMeta := &EnrichmentFileNode{
					ID:         strProp(fProps, "indexed_file_id"),
					Hash:       strProp(fProps, "hash"),
					Size:       int64Prop(fProps, "size"),
					FileType:   strProp(fProps, "file_type"),
					Entropy:    float64Prop(fProps, "entropy"),
					HasEntropy: boolProp(fProps, "has_entropy"),
				}

				// Get file-level nodes under this indexed file
				fileNeighbours, err := repository.graph.Neighbours(fn.Node.ID, graphstore.DirectionOutbound, contEdges)
				if err != nil {
					return nil, err
				}

				addedFileNode := false
				for _, file := range fileNeighbours {
					if !file.Node.HasLabel(nodeTypeFile) {
						continue
					}
					fileProps := unpackProps(file.Node.Properties)
					if strProp(fileProps, "level") != "indexed_file" {
						continue
					}

					levelFile := buildEnrichmentFileNodeFromProps(fileProps)
					mergeIndexedFallback(levelFile, indexedMeta)
					partition.Files = append(partition.Files, levelFile)
					addedFileNode = true
				}

				// Fallback for legacy/partial graph data where indexed metadata exists
				// without explicit file nodes yet.
				if !addedFileNode {
					partition.Files = append(partition.Files, indexedMeta)
				}
			}

			sort.Slice(partition.Files, func(i, j int) bool {
				if partition.Files[i].FileName == partition.Files[j].FileName {
					return partition.Files[i].Hash < partition.Files[j].Hash
				}
				return partition.Files[i].FileName < partition.Files[j].FileName
			})

			evidenceFile.Partitions = append(evidenceFile.Partitions, partition)
		}

		sort.Slice(evidenceFile.Partitions, func(i, j int) bool {
			return evidenceFile.Partitions[i].Name < evidenceFile.Partitions[j].Name
		})

		tree.EvidenceFiles = append(tree.EvidenceFiles, evidenceFile)
	}

	sort.Slice(tree.EvidenceFiles, func(i, j int) bool {
		return tree.EvidenceFiles[i].Name < tree.EvidenceFiles[j].Name
	})

	return tree, nil
}

func buildEnrichmentFileNodeFromProps(props map[string]any) *EnrichmentFileNode {
	return &EnrichmentFileNode{
		ID:           strProp(props, "file_node_id"),
		Hash:         strProp(props, "hash"),
		FileName:     strProp(props, "name"),
		Path:         strProp(props, "path"),
		Size:         int64Prop(props, "size"),
		IsDeleted:    boolProp(props, "is_deleted"),
		IsFragmented: boolProp(props, "is_fragmented"),
		FileType:     strProp(props, "file_type"),
		MimeType:     strProp(props, "mime_type"),
		Tags:         strSliceProp(props, "tags"),
		Entropy:      float64Prop(props, "entropy"),
		HasEntropy:   boolProp(props, "has_entropy"),
	}
}

func mergeIndexedFallback(fileNode, indexedMeta *EnrichmentFileNode) {
	if fileNode.Hash == "" {
		fileNode.Hash = indexedMeta.Hash
	}
	if fileNode.FileType == "" {
		fileNode.FileType = indexedMeta.FileType
	}
	if fileNode.Size == 0 {
		fileNode.Size = indexedMeta.Size
	}
	if !fileNode.HasEntropy {
		fileNode.Entropy = indexedMeta.Entropy
		fileNode.HasEntropy = indexedMeta.HasEntropy
	}
}

// ─── property helpers ────────────────────────────────────────────────────────

func unpackProps(packed []byte) map[string]any {
	var props map[string]any
	if err := msgpack.Unmarshal(packed, &props); err != nil {
		return map[string]any{}
	}
	return props
}

func strProp(props map[string]any, key string) string {
	if v, ok := props[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func int64Prop(props map[string]any, key string) int64 {
	if v, ok := props[key]; ok {
		switch val := v.(type) {
		case int64:
			return val
		case float64:
			return int64(val)
		}
	}
	return 0
}

func float64Prop(props map[string]any, key string) float64 {
	if v, ok := props[key]; ok {
		switch val := v.(type) {
		case float64:
			return val
		case float32:
			return float64(val)
		case int64:
			return float64(val)
		case int32:
			return float64(val)
		case int:
			return float64(val)
		}
	}
	return 0
}

func boolProp(props map[string]any, key string) bool {
	if v, ok := props[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return false
}

func strSliceProp(props map[string]any, key string) []string {
	if v, ok := props[key]; ok {
		switch val := v.(type) {
		case []string:
			return val
		case []interface{}:
			result := make([]string, len(val))
			for i, item := range val {
				if s, ok := item.(string); ok {
					result[i] = s
				}
			}
			return result
		}
	}
	return nil
}

func joinTags(tags []string) string {
	if len(tags) == 0 {
		return ""
	}
	return strings.Join(tags, "|")
}
