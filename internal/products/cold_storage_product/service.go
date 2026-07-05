package cold_storage_product

import (
	"context"
	"encoding/json"

	coreerr "indicer/internal/core/errors"
)

const Name = "cold_storage_product"

type Service struct{}

func New() *Service { return &Service{} }

func (s *Service) Product() string { return Name }

func (s *Service) Handle(_ context.Context, operation string, _ json.RawMessage) (any, *coreerr.Error) {
	return nil, coreerr.NewWithDetails(coreerr.CodeNotImplemented, "cold storage product is not wired in this migration slice", map[string]string{"operation": operation})
}
