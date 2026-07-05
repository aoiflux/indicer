package forensic_analysis_product

import (
	"context"
	"encoding/json"

	coreerr "indicer/internal/core/errors"
)

const Name = "forensic_analysis_product"

type Service struct{}

func New() *Service { return &Service{} }

func (s *Service) Product() string { return Name }

func (s *Service) Handle(_ context.Context, operation string, _ json.RawMessage) (any, *coreerr.Error) {
	return nil, coreerr.NewWithDetails(coreerr.CodeNotImplemented, "forensic analysis product is not wired in this migration slice", map[string]string{"operation": operation})
}
