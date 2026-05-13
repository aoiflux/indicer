package microartefact

import graphrepo "indicer/lib/microartefact/repository/graphene"

const grapheneStoreDir = graphrepo.StoreDirName

// Type aliases so callers don't need to import the inner package.
type GrapheneRepository = graphrepo.Repository
type EvidenceFileHierarchy = graphrepo.EvidenceFileHierarchy
type EvidenceFileNode = graphrepo.EvidenceFileNode
type PartitionFileNode = graphrepo.PartitionFileNode
type MicroArtefactNode = graphrepo.MicroArtefactNode
type IndexedFileArtefactView = graphrepo.IndexedFileArtefactView

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
