package api

import "context"

type GRPCAdapter struct {
	d *Dispatcher
}

func NewGRPCAdapter(d *Dispatcher) *GRPCAdapter {
	return &GRPCAdapter{d: d}
}

func (a *GRPCAdapter) Dispatch(ctx context.Context, requestJSON string) string {
	return a.d.DispatchJSON(ctx, requestJSON)
}
