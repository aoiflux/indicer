package enrichment

import (
	"path/filepath"
	"strconv"

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
)

func OpenGrapheneRepository(root string) (*GrapheneRepository, error) {
	graph, err := graphenedb.Open(filepath.Join(root, storeDirName))
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

	diskNode, err := repository.upsertNode(
		"disk_image_id",
		record.DiskImageID,
		[]graphstore.NodeType{nodeTypeDiskImage},
		map[string]any{"disk_image_id": record.DiskImageID, "name": record.DiskImageName},
	)
	if err != nil {
		return err
	}

	partitionNode, err := repository.upsertNode(
		"partition_id",
		record.PartitionID,
		[]graphstore.NodeType{nodeTypePartition},
		map[string]any{"partition_id": record.PartitionID, "name": record.PartitionName, "disk_image_id": record.DiskImageID},
	)
	if err != nil {
		return err
	}

	indexedFileID := record.ensureIndexedFileID()
	indexedNode, err := repository.upsertNode(
		"indexed_file_id",
		indexedFileID,
		[]graphstore.NodeType{nodeTypeIndexedFile},
		map[string]any{
			"indexed_file_id": indexedFileID,
			"hash":            record.Hash,
			"name":            record.Name,
			"path":            record.Path,
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

	if err := repository.graph.IndexNodeProperties(indexedNode, map[string][]byte{
		"evidence_hash": []byte(record.Hash),
		"name":          []byte(record.Name),
		"path":          []byte(record.Path),
		"file_type":     []byte(record.FileType),
		"is_deleted":    []byte(strconv.FormatBool(record.IsDeleted)),
		"is_fragmented": []byte(strconv.FormatBool(record.IsFragmented)),
		"partition_id":  []byte(record.PartitionID),
		"disk_image_id": []byte(record.DiskImageID),
	}); err != nil {
		return err
	}

	if err := repository.ensureContains(diskNode, partitionNode); err != nil {
		return err
	}
	if err := repository.ensureContains(partitionNode, indexedNode); err != nil {
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
