package repository

import "indicer/lib/microartefact/model"

type Repository interface {
	Store(file model.FileRecord, artefacts []model.Artefact, relations []model.Relation) error
	Close() error
}

type NopRepository struct{}

func (NopRepository) Store(model.FileRecord, []model.Artefact, []model.Relation) error {
	return nil
}

func (NopRepository) Close() error {
	return nil
}
