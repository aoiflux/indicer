package package_registry_product

import (
	"context"
	"encoding/json"

	coreerr "indicer/internal/core/errors"
)

const Name = "package_registry_product"

type Service struct{}

func New() *Service { return &Service{} }

func (s *Service) Product() string { return Name }

func (s *Service) Handle(_ context.Context, operation string, _ json.RawMessage) (any, *coreerr.Error) {
	return nil, coreerr.NewWithDetails(coreerr.CodeNotImplemented, "package registry product is not wired in this migration slice", map[string]string{"operation": operation})
}
