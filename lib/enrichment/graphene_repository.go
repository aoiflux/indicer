package enrichment

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	duesgraph "indicer/lib/graph"
	"indicer/lib/graph/propindex"
	"indicer/lib/graph/props"
	"indicer/lib/graph/schema"
	"indicer/lib/structs"

	graphenedb "github.com/aoiflux/graphene"
	graphstore "github.com/aoiflux/graphene/store"
)

// GrapheneRepository is a graphdb adapter backed by the existing graphene store.
//
// It holds a reference to the process-wide graph handle rather than its own
// store. The cached graph pointer stays valid for as long as that reference is
// outstanding, which is what makes it safe to keep.
type GrapheneRepository struct {
	handle *duesgraph.Handle
	graph  *graphenedb.Graph
	owns   bool

	// actor attributes every write. The zero value writes unattributed, which
	// is what DUES did before batches existed; the signer provider populates it.
	actor duesgraph.ActorRef
}

// ErrRepositoryClosed is returned by a write, or a read, through a repository
// that is not open. Writes used to succeed doing nothing (GRA-10).
var ErrRepositoryClosed = errors.New("enrichment: graph repository is not open")

// kinds resolves node kinds to the raw type values this particular store uses.
//
// The package-level vars this replaced were derived once from
// graphstore.CustomNodeType, which pinned them to whatever base the linked
// library happened to have. When graphene moved that base from 128 to 32768 the
// derived values silently stopped matching everything already on disk. Kinds
// are offsets now, and only a store's own registry turns them into values.
func (repository *GrapheneRepository) kinds() *schema.Registry {
	return repository.handle.Schema()
}

// OpenGrapheneRepository acquires a reference to root's graph store,
// creating it if needed. Close releases it.
func OpenGrapheneRepository(root string) (*GrapheneRepository, error) {
	return OpenGrapheneRepositoryWith(root, duesgraph.OpenOptions{})
}

// OpenGrapheneRepositoryReadOnly opens root's graph store for reading. A data
// root without one is duesgraph.ErrNoGraph, and nothing is created.
func OpenGrapheneRepositoryReadOnly(root string) (*GrapheneRepository, error) {
	return OpenGrapheneRepositoryWith(root, duesgraph.OpenOptions{ReadOnly: true})
}

func OpenGrapheneRepositoryWith(root string, opts duesgraph.OpenOptions) (*GrapheneRepository, error) {
	handle, err := duesgraph.OpenWith(root, opts)
	if err != nil {
		return nil, err
	}
	return &GrapheneRepository{handle: handle, graph: handle.Graph(), owns: true}, nil
}

// NewGrapheneRepositoryOn borrows a handle a caller already holds. Close is a
// no-op on the result: the reference belongs to whoever opened it.
func NewGrapheneRepositoryOn(handle *duesgraph.Handle) *GrapheneRepository {
	if handle == nil {
		return nil
	}
	return &GrapheneRepository{handle: handle, graph: handle.Graph()}
}

// SetActor records who subsequent writes are attributed to. graphene binds an
// actor to a batch commit and to nothing else, so this has to be set before the
// writes it should cover — attribution cannot be added retrospectively.
func (repository *GrapheneRepository) SetActor(actor duesgraph.ActorRef) {
	if repository != nil {
		repository.actor = actor
	}
}

// producerVersion identifies this enrichment pipeline on every node it derives.
//
// Bump it whenever a change alters what enrichment *produces* from the same
// evidence — a new property, a changed derivation — so a re-derivation can be
// diffed against what an earlier version wrote. It is not the DUES release
// version, which moves for reasons that do not affect derived data.
const producerVersion = "1"

// inBatch runs fn inside one attributed, atomic commit.
func (repository *GrapheneRepository) inBatch(fn func(*duesgraph.Batch) error) error {
	writer, err := repository.handle.Writer(repository.actor)
	if err != nil {
		return err
	}
	writer.As(duesgraph.Producer{Pipeline: "enrichment", Version: producerVersion})
	defer writer.Close()

	batch := writer.Batch()
	if err := fn(batch); err != nil {
		batch.Rollback()
		return err
	}
	return batch.Commit()
}

func (repository *GrapheneRepository) UpsertEvidence(record EvidenceRecord) error {
	if repository == nil || repository.graph == nil {
		return ErrRepositoryClosed
	}
	return repository.inBatch(func(batch *duesgraph.Batch) error {
		return repository.upsertEvidence(batch, record)
	})
}

func (repository *GrapheneRepository) upsertEvidence(batch *duesgraph.Batch, record EvidenceRecord) error {
	_, err := batch.Upsert(
		duesgraph.UpsertKey{Key: "evidence_file_id", Value: record.HashBase64},
		[]graphstore.NodeType{repository.kinds().NodeType(schema.KindEvidenceFile)},
		props.Map{
			"evidence_file_id": record.HashBase64,
			"name":             record.Name,
			"path":             record.Path,
			"size":             record.Size,
			"evidence_hash":    record.HashBase64,
			"node_type":        "evidence",
			"evidence_type":    record.EvidenceType,
			"ingest_state":     record.IngestState,
			"completed":        record.Completed,
			"failed":           record.Failed,
			"partition_count":  record.PartitionCount,
		},
		// The full record replaces a stub UpsertPartition or upsertFile wrote
		// knowing only the ID, index entries included (GRA-2).
		map[string][]byte{
			"evidence_hash": []byte(record.HashBase64),
			"name":          []byte(record.Name),
			"path":          []byte(record.Path),
			"evidence_type": []byte(record.EvidenceType),
			"ingest_state":  []byte(record.IngestState),
			"size":          propindex.Int64(record.Size),
		},
	)
	return err
}

func (repository *GrapheneRepository) UpsertPartition(record PartitionRecord) error {
	if repository == nil || repository.graph == nil {
		return ErrRepositoryClosed
	}
	return repository.inBatch(func(batch *duesgraph.Batch) error {
		return repository.upsertPartition(batch, record)
	})
}

func (repository *GrapheneRepository) upsertPartition(batch *duesgraph.Batch, record PartitionRecord) error {
	evidenceNode, err := repository.upsertEvidenceStub(batch, record.EvidenceFileID, record.EvidenceFileName)
	if err != nil {
		return err
	}

	partitionNode, err := batch.Upsert(
		duesgraph.UpsertKey{Key: "partition_id", Value: record.PartitionID},
		[]graphstore.NodeType{repository.kinds().NodeType(schema.KindPartition)},
		props.Map{
			"partition_id":       record.PartitionID,
			"name":               record.PartitionName,
			"evidence_file_id":   record.EvidenceFileID,
			"start":              record.Offset,
			"size":               record.Size,
			"filesystem_type":    record.FilesystemType,
			"hash":               record.Hash,
			"indexed_file_count": record.IndexedFileCount,
		},
		// Replaces the thin stub upsertFile creates when it walks a file
		// before this runs.
		map[string][]byte{
			"name":            []byte(record.PartitionName),
			"filesystem_type": []byte(record.FilesystemType),
			"size":            propindex.Int64(record.Size),
		},
	)
	if err != nil {
		return err
	}

	return repository.ensureContains(batch, evidenceNode, partitionNode)
}

// upsertEvidenceStub creates the evidence node knowing only its ID and name.
// UpsertEvidence fills in the rest when it runs; ordering between them is not
// guaranteed, which is why both register their index keys unconditionally.
func (repository *GrapheneRepository) upsertEvidenceStub(
	batch *duesgraph.Batch, evidenceFileID, evidenceFileName string,
) (graphstore.NodeID, error) {
	return batch.UpsertFill(
		duesgraph.UpsertKey{Key: "evidence_file_id", Value: evidenceFileID},
		[]graphstore.NodeType{repository.kinds().NodeType(schema.KindEvidenceFile)},
		props.Map{"evidence_file_id": evidenceFileID, "name": evidenceFileName},
		nil,
	)
}

func (repository *GrapheneRepository) UpsertFile(record FileRecord) error {
	if repository == nil || repository.graph == nil {
		return ErrRepositoryClosed
	}
	return repository.inBatch(func(batch *duesgraph.Batch) error {
		return repository.upsertFile(batch, record)
	})
}

// UpsertFiles writes many file records in one commit.
//
// One commit per file is one fsync per file, and one attribution record per
// file. Callers walking a partition should prefer this: EnrichAll on a large
// image is the difference between a commit per file and a commit per partition.
func (repository *GrapheneRepository) UpsertFiles(records []FileRecord) error {
	if repository == nil || repository.graph == nil {
		return ErrRepositoryClosed
	}
	if len(records) == 0 {
		return nil
	}
	return repository.inBatch(func(batch *duesgraph.Batch) error {
		for _, record := range records {
			if err := repository.upsertFile(batch, record); err != nil {
				return err
			}
		}
		return nil
	})
}

func (repository *GrapheneRepository) upsertFile(batch *duesgraph.Batch, record FileRecord) error {
	// Always create/get evidence file node
	evidenceNode, err := repository.upsertEvidenceStub(batch, record.EvidenceFileID, record.EvidenceFileName)
	if err != nil {
		return err
	}

	var parentNode graphstore.NodeID = evidenceNode

	// For partition and indexed_file levels, create/get partition node
	if record.Level != "evidence_file" && record.PartitionID != "" {
		partitionNode, err := batch.UpsertFill(
			duesgraph.UpsertKey{Key: "partition_id", Value: record.PartitionID},
			[]graphstore.NodeType{repository.kinds().NodeType(schema.KindPartition)},
			props.Map{"partition_id": record.PartitionID, "name": record.PartitionName, "evidence_file_id": record.EvidenceFileID},
			nil,
		)
		if err != nil {
			return err
		}
		if err := repository.ensureContains(batch, evidenceNode, partitionNode); err != nil {
			return err
		}
		parentNode = partitionNode

		// For indexed_file level, create/get indexed file node
		if record.Level == "indexed_file" && record.IndexedFileID != "" {
			indexedProps := props.Map{
				"indexed_file_id":  record.IndexedFileID,
				"hash":             record.IndexedFileHash,
				"size":             record.Size,
				"partition_id":     record.PartitionID,
				"evidence_file_id": record.EvidenceFileID,
				"file_type":        record.FileType,
				"has_entropy":      record.HasEntropy,
				// The indexed node previously carried none of these: its byte
				// offset, and whether it was recovered from a deleted entry or
				// spans discontiguous runs. Those live on the FILE node, which
				// is a different node from the one a traversal lands on.
				"start":         record.Start,
				"is_deleted":    record.IsDeleted,
				"is_fragmented": record.IsFragmented,
				"has_simhash":   record.HasSimhash,
			}
			if record.HasEntropy {
				indexedProps["entropy"] = record.Entropy
			}
			if record.HasSimhash {
				indexedProps["simhash"] = record.Simhash
			}
			timeProps(indexedProps, record.Times)

			indexedIndex := map[string][]byte{
				"size":      propindex.Int64(record.Size),
				"file_type": []byte(record.FileType),
			}
			if record.HasEntropy {
				indexedIndex["entropy"] = propindex.Float64(record.Entropy)
			}
			if record.HasSimhash {
				indexedIndex["simhash"] = propindex.Uint64(record.Simhash)
			}
			addTimeIndex(indexedIndex, record.Times)

			indexedNode, err := batch.Upsert(
				duesgraph.UpsertKey{Key: "indexed_file_id", Value: record.IndexedFileID},
				[]graphstore.NodeType{repository.kinds().NodeType(schema.KindIndexedFile)},
				indexedProps,
				indexedIndex,
			)
			if err != nil {
				return err
			}
			if err := repository.ensureContains(batch, partitionNode, indexedNode); err != nil {
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
	fileProps := props.Map{
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
		"start":            record.Start,
		"has_simhash":      record.HasSimhash,
	}
	if record.HasSimhash {
		fileProps["simhash"] = record.Simhash
	}
	if record.HasEntropy {
		fileProps["entropy"] = record.Entropy
	}
	timeProps(fileProps, record.Times)
	// strconv encodings, deliberately unchanged. These are equality-only index
	// values today; switching them to the order-preserving encoders would make
	// new entries incomparable with the ones already stored. They are re-encoded
	// by the rebuild migration, which is also what makes ordered range queries
	// on them possible.
	fileIndex := map[string][]byte{
		"name":          []byte(record.FileName),
		"level":         []byte(record.Level),
		"is_deleted":    []byte(strconv.FormatBool(record.IsDeleted)),
		"is_fragmented": []byte(strconv.FormatBool(record.IsFragmented)),
		"mime_type":     []byte(record.MimeType),
		"tags":          []byte(joinTags(record.Tags)),
	}
	// Timestamps, size, entropy and simhash are new or previously unindexed
	// keys, so unlike the above they are written order-preserving from the
	// start and can back range queries without waiting for a re-encode.
	addTimeIndex(fileIndex, record.Times)
	fileIndex["size"] = propindex.Int64(record.Size)
	if record.HasEntropy {
		fileIndex["entropy"] = propindex.Float64(record.Entropy)
	}
	if record.HasSimhash {
		fileIndex["simhash"] = propindex.Uint64(record.Simhash)
	}
	if record.Path != "" {
		fileIndex["path"] = propindex.String(record.Path)
	}
	fileNode, err := batch.Upsert(
		duesgraph.UpsertKey{Key: "file_node_id", Value: fileNodeID},
		[]graphstore.NodeType{repository.kinds().NodeType(schema.KindFile)},
		fileProps,
		fileIndex,
	)
	if err != nil {
		return err
	}

	// Create edge from parent to file
	return repository.ensureContains(batch, parentNode, fileNode)
}

// Close releases this repository's reference to the graph handle. The store
// itself closes when the last reference in the process goes.
func (repository *GrapheneRepository) Close() error {
	if repository == nil || !repository.owns {
		return nil
	}
	repository.owns = false
	repository.graph = nil
	handle := repository.handle
	repository.handle = nil
	return handle.Release()
}

// timeProps adds the MACB timestamps to a node's property blob as Unix
// nanoseconds. Absent times are simply not written, so a consumer can tell
// "this filesystem does not record a birth time" from "the birth time is the
// epoch" — a distinction that matters on ext, exFAT and FAT.
func timeProps(into props.Map, times *structs.Timestamps) {
	if !times.Any() {
		return
	}
	for key, value := range map[string]*int64{
		"mtime": times.Modified,
		"atime": times.Accessed,
		"ctime": times.Changed,
		"btime": times.Born,
	} {
		if value != nil {
			into[key] = *value
		}
	}
}

// addTimeIndex registers the timestamps as order-preserving index values, so a
// timeline window resolves through two binary searches rather than a scan.
func addTimeIndex(into map[string][]byte, times *structs.Timestamps) {
	if !times.Any() {
		return
	}
	for key, value := range map[string]*int64{
		"mtime": times.Modified,
		"atime": times.Accessed,
		"ctime": times.Changed,
		"btime": times.Born,
	} {
		if value != nil {
			into[key] = propindex.Int64(*value)
		}
	}
}

func (repository *GrapheneRepository) ensureContains(batch *duesgraph.Batch, src, dst graphstore.NodeID) error {
	return batch.EnsureEdge(src, dst, []graphstore.EdgeType{graphstore.EdgeTypeContains}, nil, nil)
}

// ReadHierarchy walks the enrichment graphdb and returns the full
// evidence_file → partition → indexed_file hierarchy with file-level metadata.
func (repository *GrapheneRepository) ReadHierarchy() (*EnrichmentHierarchy, error) {
	if repository == nil || repository.graph == nil {
		return nil, ErrRepositoryClosed
	}

	tree := &EnrichmentHierarchy{}
	kinds := repository.kinds()

	// NodesByAnyType over every encoding this store uses, rather than
	// NodesByType over one: on a store written by graphene v0.1.3 the current
	// encoding matches nothing, and the query would return an empty hierarchy
	// without raising an error.
	evidenceFileIDs, err := repository.graph.NodesByAnyType(kinds.MatchTypes(schema.KindEvidenceFile))
	if err != nil {
		return nil, err
	}

	contEdges := []graphstore.EdgeType{graphstore.EdgeTypeContains}

	for _, evidenceID := range evidenceFileIDs {
		evidenceNode, err := repository.graph.GetNode(evidenceID)
		if err != nil {
			return nil, err
		}
		dProps, err := unpackProps(evidenceNode)
		if err != nil {
			return nil, err
		}
		evidenceFile := &EnrichmentEvidenceFileNode{
			ID:   strProp(dProps, "evidence_file_id"),
			Name: strProp(dProps, "name"),
		}

		partNeighbours, err := repository.graph.Neighbours(evidenceID, graphstore.DirectionOutbound, contEdges)
		if err != nil {
			return nil, err
		}

		for _, pn := range partNeighbours {
			if !kinds.Is(pn.Node, schema.KindPartition) {
				continue
			}
			pProps, err := unpackProps(pn.Node)
			if err != nil {
				return nil, err
			}
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
				if !kinds.Is(fn.Node, schema.KindIndexedFile) {
					continue
				}
				fProps, err := unpackProps(fn.Node)
				if err != nil {
					return nil, err
				}
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
					if !kinds.Is(file.Node, schema.KindFile) {
						continue
					}
					fileProps, err := unpackProps(file.Node)
					if err != nil {
						return nil, err
					}
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

func buildEnrichmentFileNodeFromProps(fields props.Map) *EnrichmentFileNode {
	return &EnrichmentFileNode{
		ID:           fields.Str("file_node_id"),
		Hash:         fields.Str("hash"),
		FileName:     fields.Str("name"),
		Path:         fields.Str("path"),
		Size:         fields.Int64("size"),
		IsDeleted:    fields.Bool("is_deleted"),
		IsFragmented: fields.Bool("is_fragmented"),
		FileType:     fields.Str("file_type"),
		MimeType:     fields.Str("mime_type"),
		Tags:         fields.Strings("tags"),
		Entropy:      fields.Float64("entropy"),
		HasEntropy:   fields.Bool("has_entropy"),
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
//
// These now delegate to lib/graph/props. The local copies they replaced read
// size fields back as 0 whenever msgpack had encoded them as uint64, int8 or
// int16 — which it does for most real values — and rejected the string form of
// a bool. See the package doc for the full drift.

// unpackProps decodes a node's properties, naming the node when they do not
// decode (GRA-10).
func unpackProps(node *graphstore.Node) (props.Map, error) {
	fields, err := props.Unpack(node.Properties)
	if err != nil {
		return nil, fmt.Errorf("graph node %d: %w", node.ID, err)
	}
	return fields, nil
}

func strProp(m props.Map, key string) string      { return m.Str(key) }
func int64Prop(m props.Map, key string) int64     { return m.Int64(key) }
func float64Prop(m props.Map, key string) float64 { return m.Float64(key) }
func boolProp(m props.Map, key string) bool       { return m.Bool(key) }

func joinTags(tags []string) string {
	if len(tags) == 0 {
		return ""
	}
	return strings.Join(tags, "|")
}

// deleteBatchSize bounds how many nodes one commit deletes.
const deleteBatchSize = 10000

// DeleteEvidenceIndex removes from the graph what was derived from an
// evidence's index: its partitions, their indexed files, the file nodes under
// them and the micro-artefacts under those, with every edge they have. A
// reindex rebuilds the index under new records, a duplicate's index is
// removed, and a quarantined index is hidden; each left the old nodes in the
// graph beside the current ones. keepEvidence keeps the evidence node and the
// file node describing the evidence itself. It returns how many nodes it
// deleted.
func (repository *GrapheneRepository) DeleteEvidenceIndex(evidenceID string, keepEvidence bool) (int, error) {
	if repository == nil || repository.graph == nil {
		return 0, ErrRepositoryClosed
	}
	kinds := repository.kinds()
	contains := []graphstore.EdgeType{graphstore.EdgeTypeContains}

	roots, err := repository.graph.NodesByProperty("evidence_file_id", []byte(evidenceID))
	if err != nil {
		return 0, err
	}
	doomed := map[graphstore.NodeID]struct{}{}
	var order, queue []graphstore.NodeID
	mark := func(id graphstore.NodeID) {
		if _, seen := doomed[id]; !seen {
			doomed[id] = struct{}{}
			order = append(order, id)
			queue = append(queue, id)
		}
	}
	for _, id := range roots {
		node, err := repository.graph.GetNode(id)
		if err != nil {
			return 0, err
		}
		if !kinds.Is(node, schema.KindEvidenceFile) {
			continue
		}
		if !keepEvidence {
			mark(id)
			continue
		}
		children, err := repository.graph.Neighbours(id, graphstore.DirectionOutbound, contains)
		if err != nil {
			return 0, err
		}
		for _, child := range children {
			if child.Node == nil {
				continue
			}
			if kinds.Is(child.Node, schema.KindFile) {
				fields, err := unpackProps(child.Node)
				if err != nil {
					return 0, err
				}
				if fields.Str("level") == "evidence_file" {
					continue
				}
			}
			mark(child.Node.ID)
		}
	}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		children, err := repository.graph.Neighbours(id, graphstore.DirectionOutbound, contains)
		if err != nil {
			return 0, err
		}
		for _, child := range children {
			if child.Node != nil {
				mark(child.Node.ID)
			}
		}
	}

	for start := 0; start < len(order); start += deleteBatchSize {
		part := order[start:min(start+deleteBatchSize, len(order))]
		err := repository.inBatch(func(batch *duesgraph.Batch) error {
			for _, id := range part {
				batch.DeleteNode(id)
			}
			return nil
		})
		if err != nil {
			return start, err
		}
	}
	return len(order), nil
}

// FileAnnotation is what enrichment derived for one indexed file.
type FileAnnotation struct {
	MimeType   string
	Tags       []string
	Entropy    float64
	HasEntropy bool
}

// IndexedFileAnnotations returns what enrichment derived for each indexed
// file the graph holds, by indexed-file ID.
func (repository *GrapheneRepository) IndexedFileAnnotations() (map[string]FileAnnotation, error) {
	if repository == nil || repository.graph == nil {
		return nil, ErrRepositoryClosed
	}
	ids, err := repository.graph.NodesByProperty("level", []byte("indexed_file"))
	if err != nil {
		return nil, err
	}
	out := make(map[string]FileAnnotation, len(ids))
	for _, id := range ids {
		node, err := repository.graph.GetNode(id)
		if err != nil {
			return nil, err
		}
		fields, err := unpackProps(node)
		if err != nil {
			return nil, err
		}
		iid := fields.Str("indexed_file_id")
		if iid == "" {
			continue
		}
		out[iid] = FileAnnotation{
			MimeType:   fields.Str("mime_type"),
			Tags:       fields.Strings("tags"),
			Entropy:    fields.Float64("entropy"),
			HasEntropy: fields.Bool("has_entropy"),
		}
	}
	return out, nil
}
