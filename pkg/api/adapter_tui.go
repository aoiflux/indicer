package api

import "context"

type TUIAdapter struct {
	d *Dispatcher
}

func NewTUIAdapter(d *Dispatcher) *TUIAdapter {
	return &TUIAdapter{d: d}
}

func (a *TUIAdapter) Dispatch(ctx context.Context, requestJSON string) string {
	return a.d.DispatchJSON(ctx, requestJSON)
}
