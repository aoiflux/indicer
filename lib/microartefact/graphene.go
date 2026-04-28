package microartefact

import graphrepo "indicer/lib/microartefact/repository/graphene"

const grapheneStoreDir = graphrepo.StoreDirName

// Type aliases so callers don't need to import the inner package.
type GrapheneRepository = graphrepo.Repository
type HierarchyTree = graphrepo.HierarchyTree
type DiskImageNode = graphrepo.DiskImageNode
type PartitionNode = graphrepo.PartitionNode
type FileNode = graphrepo.FileNode
type MicroArtefactNode = graphrepo.MicroArtefactNode

// Legacy aliases kept for compatibility.
type IndexedFileNode = graphrepo.FileNode
type ArtefactNode = graphrepo.MicroArtefactNode

func OpenGrapheneRepository(root string) (*GrapheneRepository, error) {
	return graphrepo.Open(root)
}

func NewGrapheneService(root string) (*Service, error) {
	repository, err := graphrepo.Open(root)
	if err != nil {
		return nil, err
	}
	return NewService(repository), nil
}
