package cli

import (
	"fmt"
	"indicer/lib/enrichment"
)

func EnrichData(chonkSize int, dbpath string, key []byte) error {
	db, dbpath, err := Common(chonkSize, dbpath, key)
	if err != nil {
		return err
	}
	defer db.Close()

	repo, err := enrichment.OpenGrapheneRepository(dbpath)
	if err != nil {
		return err
	}

	service := enrichment.NewService(db, repo)
	defer service.Close()

	if err := service.EnrichAll(); err != nil {
		return err
	}

	fmt.Println("Graph enrichment complete: disk images, partitions, and indexed-file metadata upserted.")
	return nil
}
