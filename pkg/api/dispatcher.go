package api

import (
	"context"

	coreerr "indicer/internal/core/errors"
	"indicer/internal/core/jsonbridge"
	"indicer/internal/products/backup_product"
	"indicer/internal/products/cold_storage_product"
	"indicer/internal/products/compression_product"
	"indicer/internal/products/evidence_management_product"
	"indicer/internal/products/forensic_analysis_product"
	"indicer/internal/products/package_registry_product"
	"indicer/internal/products/registry"
)

type Dispatcher struct {
	registry *registry.Registry
}

func NewDispatcher(r *registry.Registry) *Dispatcher {
	return &Dispatcher{registry: r}
}

func NewDefaultDispatcher() *Dispatcher {
	r := registry.New()
	r.Register(compression_product.New())
	r.Register(backup_product.New())
	r.Register(package_registry_product.New())
	r.Register(evidence_management_product.New())
	r.Register(forensic_analysis_product.New())
	r.Register(cold_storage_product.New())
	return NewDispatcher(r)
}

var defaultDispatcher = NewDefaultDispatcher()

func DefaultDispatcher() *Dispatcher {
	return defaultDispatcher
}

func (d *Dispatcher) DispatchJSON(ctx context.Context, input string) string {
	req, reqErr := jsonbridge.DecodeRequest(input)
	if reqErr != nil {
		return jsonbridge.EncodeError(reqErr)
	}

	service, ok := d.registry.Get(req.Product)
	if !ok {
		return jsonbridge.EncodeError(coreerr.NewWithDetails(coreerr.CodeUnknownProduct, "unknown product", map[string]string{"product": req.Product}))
	}

	result, runErr := service.Handle(ctx, req.Operation, req.Params)
	if runErr != nil {
		return jsonbridge.EncodeError(runErr)
	}

	return jsonbridge.EncodeOK(result)
}
