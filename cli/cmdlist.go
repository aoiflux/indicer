package cli

import (
	"indicer/lib/enrichment"
	"indicer/lib/store"
)

func ListData(chonkSize int, dbpath string, key []byte) error {
	db, _, err := Common(chonkSize, dbpath, key)
	if err != nil {
		return err
	}
	defer db.Close()

	// Try to open enrichment graphdb for graphdb-first retrieval
	enrichRepo, err := enrichment.OpenGrapheneRepository(dbpath)
	if err != nil {
		// Graphdb not available, fall back to KVDB-only
		return store.List(db)
	}
	defer enrichRepo.Close()

	// Try graphdb-first listing
	return store.ListWithEnrichment(db, enrichRepo)
}
