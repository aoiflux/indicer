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
	nodeTypeDiskImage   = graphstore.CustomNodeType(0)
	nodeTypePartition   = graphstore.CustomNodeType(1)
	nodeTypeIndexedFile = graphstore.CustomNodeType(2)
	nodeTypeFile        = graphstore.CustomNodeType(3)
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

	diskNode, err := repository.upsertNode(
		"disk_image_id",
		record.HashBase64,
		[]graphstore.NodeType{nodeTypeDiskImage},
		map[string]any{
			"disk_image_id": record.HashBase64,
			"name":          record.Name,
			"path":          record.Path,
			"size":          record.Size,
			"evidence_hash": record.HashBase64,
			"node_type":     "evidence",
		},
	)
	if err != nil {
		return err
	}

	if err := repository.graph.IndexNodeProperties(diskNode, map[string][]byte{
		"evidence_hash": []byte(record.HashBase64),
		"name":          []byte(record.Name),
		"path":          []byte(record.Path),
	}); err != nil {
		return err
	}

	return nil
}

func (repository *GrapheneRepository) UpsertFile(record FileRecord) error {
	if repository == nil || repository.graph == nil {
		return nil
	}

	// Always create/get disk image node
	diskNode, err := repository.upsertNode(
		"disk_image_id",
		record.DiskImageID,
		[]graphstore.NodeType{nodeTypeDiskImage},
		map[string]any{"disk_image_id": record.DiskImageID, "name": record.DiskImageName},
	)
	if err != nil {
		return err
	}

	var parentNode graphstore.NodeID = diskNode

	// For partition and indexed_file levels, create/get partition node
	if record.Level != "disk_image" && record.PartitionID != "" {
		partitionNode, err := repository.upsertNode(
			"partition_id",
			record.PartitionID,
			[]graphstore.NodeType{nodeTypePartition},
			map[string]any{"partition_id": record.PartitionID, "name": record.PartitionName, "disk_image_id": record.DiskImageID},
		)
		if err != nil {
			return err
		}
		if err := repository.ensureContains(diskNode, partitionNode); err != nil {
			return err
		}
		parentNode = partitionNode

		// For indexed_file level, create/get indexed file node
		if record.Level == "indexed_file" && record.IndexedFileID != "" {
			indexedFileNames := []string{}
			if record.FileName != "" {
				indexedFileNames = append(indexedFileNames, record.FileName)
			}
			indexedNode, err := repository.upsertNode(
				"indexed_file_id",
				record.IndexedFileID,
				[]graphstore.NodeType{nodeTypeIndexedFile},
				map[string]any{
					"indexed_file_id": record.IndexedFileID,
					"hash":            record.IndexedFileHash,
					"file_names":      indexedFileNames,
					"size":            record.Size,
					"is_deleted":      record.IsDeleted,
					"is_fragmented":   record.IsFragmented,
					"partition_id":    record.PartitionID,
					"disk_image_id":   record.DiskImageID,
					"file_type":       record.FileType,
				},
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
	fileNode, err := repository.upsertNode(
		"file_node_id",
		fileNodeID,
		[]graphstore.NodeType{nodeTypeFile},
		map[string]any{
			"file_node_id":  fileNodeID,
			"name":          record.FileName,
			"path":          record.Path,
			"size":          record.Size,
			"is_deleted":    record.IsDeleted,
			"is_fragmented": record.IsFragmented,
			"level":         record.Level,
			"mime_type":     record.MimeType,
			"tags":          record.Tags,
		},
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
// disk_image → partition → indexed_file hierarchy with file-level metadata.
func (repository *GrapheneRepository) ReadHierarchy() (*EnrichmentHierarchy, error) {
	if repository == nil || repository.graph == nil {
		return &EnrichmentHierarchy{}, nil
	}

	tree := &EnrichmentHierarchy{}
	diskImageIDs, err := repository.graph.NodesByType(nodeTypeDiskImage)
	if err != nil {
		return nil, err
	}

	contEdges := []graphstore.EdgeType{graphstore.EdgeTypeContains}

	for _, diskID := range diskImageIDs {
		diskNode, err := repository.graph.GetNode(diskID)
		if err != nil {
			return nil, err
		}
		dProps := unpackProps(diskNode.Properties)
		di := &EnrichmentDiskImageNode{
			ID:   strProp(dProps, "disk_image_id"),
			Name: strProp(dProps, "name"),
		}

		// Get disk image level files
		diskImageFileMap := make(map[string]*EnrichmentFileNode)
		diskFileNeighbours, err := repository.graph.Neighbours(diskID, graphstore.DirectionOutbound, contEdges)
		if err != nil {
			return nil, err
		}
		for _, dfn := range diskFileNeighbours {
			if !dfn.Node.HasLabel(nodeTypeFile) {
				continue
			}
			dfProps := unpackProps(dfn.Node.Properties)
			fileLevel := strProp(dfProps, "level")
			if fileLevel == "disk_image" {
				dfile := &EnrichmentFileNode{
					ID:           strProp(dfProps, "file_node_id"),
					FileName:     strProp(dfProps, "name"),
					Path:         strProp(dfProps, "path"),
					Size:         int64Prop(dfProps, "size"),
					IsDeleted:    boolProp(dfProps, "is_deleted"),
					IsFragmented: boolProp(dfProps, "is_fragmented"),
					MimeType:     strProp(dfProps, "mime_type"),
					Tags:         strSliceProp(dfProps, "tags"),
				}
				diskImageFileMap[dfile.ID] = dfile
			}
		}

		partNeighbours, err := repository.graph.Neighbours(diskID, graphstore.DirectionOutbound, contEdges)
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

			// Get partition level files
			partitionFileMap := make(map[string]*EnrichmentFileNode)
			partFileNeighbours, err := repository.graph.Neighbours(pn.Node.ID, graphstore.DirectionOutbound, contEdges)
			if err != nil {
				return nil, err
			}
			for _, pfn := range partFileNeighbours {
				if !pfn.Node.HasLabel(nodeTypeFile) {
					continue
				}
				pfProps := unpackProps(pfn.Node.Properties)
				fileLevel := strProp(pfProps, "level")
				if fileLevel == "partition" {
					pfile := &EnrichmentFileNode{
						ID:           strProp(pfProps, "file_node_id"),
						FileName:     strProp(pfProps, "name"),
						Path:         strProp(pfProps, "path"),
						Size:         int64Prop(pfProps, "size"),
						IsDeleted:    boolProp(pfProps, "is_deleted"),
						IsFragmented: boolProp(pfProps, "is_fragmented"),
						MimeType:     strProp(pfProps, "mime_type"),
						Tags:         strSliceProp(pfProps, "tags"),
					}
					partitionFileMap[pfile.ID] = pfile
				}
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
				ifile := &EnrichmentFileNode{
					ID:           strProp(fProps, "indexed_file_id"),
					Hash:         strProp(fProps, "hash"),
					FileNames:    strSliceProp(fProps, "file_names"),
					Size:         int64Prop(fProps, "size"),
					IsDeleted:    boolProp(fProps, "is_deleted"),
					IsFragmented: boolProp(fProps, "is_fragmented"),
					FileType:     strProp(fProps, "file_type"),
				}

				// Get file-level nodes under this indexed file
				fileNeighbours, err := repository.graph.Neighbours(fn.Node.ID, graphstore.DirectionOutbound, contEdges)
				if err != nil {
					return nil, err
				}

				var indexedFileLevelFiles []*EnrichmentFileNode
				for _, file := range fileNeighbours {
					if !file.Node.HasLabel(nodeTypeFile) {
						continue
					}
					fileProps := unpackProps(file.Node.Properties)
					fileLevel := strProp(fileProps, "level")
					if fileLevel == "indexed_file" {
						levelFile := &EnrichmentFileNode{
							ID:           strProp(fileProps, "file_node_id"),
							FileName:     strProp(fileProps, "name"),
							Path:         strProp(fileProps, "path"),
							Size:         int64Prop(fileProps, "size"),
							IsDeleted:    boolProp(fileProps, "is_deleted"),
							IsFragmented: boolProp(fileProps, "is_fragmented"),
							MimeType:     strProp(fileProps, "mime_type"),
							Tags:         strSliceProp(fileProps, "tags"),
						}
						indexedFileLevelFiles = append(indexedFileLevelFiles, levelFile)
					}
				}

				sort.Slice(indexedFileLevelFiles, func(i, j int) bool {
					return indexedFileLevelFiles[i].FileName < indexedFileLevelFiles[j].FileName
				})

				ifile.FileLevelNodes = indexedFileLevelFiles
				if len(indexedFileLevelFiles) > 0 {
					ifile.FileNames = make([]string, len(indexedFileLevelFiles))
					for i, f := range indexedFileLevelFiles {
						ifile.FileNames[i] = f.FileName
					}
				}

				partition.Files = append(partition.Files, ifile)
			}

			sort.Slice(partition.Files, func(i, j int) bool {
				return partition.Files[i].Hash < partition.Files[j].Hash
			})

			di.Partitions = append(di.Partitions, partition)
		}

		sort.Slice(di.Partitions, func(i, j int) bool {
			return di.Partitions[i].Name < di.Partitions[j].Name
		})

		tree.EvidenceFiles = append(tree.EvidenceFiles, di)
	}

	sort.Slice(tree.EvidenceFiles, func(i, j int) bool {
		return tree.EvidenceFiles[i].Name < tree.EvidenceFiles[j].Name
	})

	return tree, nil
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
