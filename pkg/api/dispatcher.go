package api

import (
	"context"
	"sync/atomic"
	"time"

	coreerr "indicer/internal/core/errors"
	"indicer/internal/core/jsonbridge"
	"indicer/internal/products/compression_product"
	"indicer/internal/products/registry"
	"indicer/internal/safe"
)

type Dispatcher struct {
	registry *registry.Registry
	closed   atomic.Bool
}

func NewDispatcher(r *registry.Registry) *Dispatcher {
	return &Dispatcher{registry: r}
}

func NewDefaultDispatcher() *Dispatcher {
	r := registry.New()
	compressionService := compression_product.New()
	r.Register(compressionService)
	r.RegisterAs(compression_product.LegacyName, compressionService)
	return NewDispatcher(r)
}

var defaultDispatcher = NewDefaultDispatcher()

func DefaultDispatcher() *Dispatcher {
	return defaultDispatcher
}

// DispatchJSON runs one request envelope and returns the response envelope.
// It never panics and never returns an empty string: a panic in a service,
// or a result that cannot be encoded, is an internal_error.
func (d *Dispatcher) DispatchJSON(ctx context.Context, input string) string {
	req, reqErr := jsonbridge.DecodeRequest(input)
	if reqErr != nil {
		return jsonbridge.EncodeError(reqErr)
	}
	if d.closed.Load() {
		return jsonbridge.EncodeError(shutDown())
	}

	service, ok := d.registry.Get(req.Product)
	if !ok {
		return jsonbridge.EncodeError(coreerr.NewWithDetails(coreerr.CodeUnknownProduct, "unknown product", map[string]string{"product": req.Product}))
	}

	if req.TimeoutMS > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(req.TimeoutMS)*time.Millisecond)
		defer cancel()
	}

	site := safe.Site{Component: "api/" + req.Product, Op: req.Operation}
	out, err := safe.Value(site, func() (string, error) {
		result, runErr := service.Handle(ctx, req.Operation, req.Params)
		if runErr != nil {
			// Once the context has ended, a failure is the cancellation's.
			if ctx.Err() != nil && runErr.Code != coreerr.CodeCanceled {
				runErr = coreerr.FromContext(ctx)
			}
			return jsonbridge.EncodeError(runErr), nil
		}
		return jsonbridge.EncodeOK(result), nil
	})
	if err != nil {
		return jsonbridge.EncodeError(coreerr.FromPanic(err))
	}
	return out
}

// ShutdownResult says what Shutdown stopped.
type ShutdownResult struct {
	CanceledTasks int `json:"canceled_tasks"`
	RunningTasks  int `json:"running_tasks"`
}

// Shutdown refuses every later request, cancels the services' background
// tasks and waits for them until ctx ends. RunningTasks counts the tasks
// still running when it returned.
func (d *Dispatcher) Shutdown(ctx context.Context) ShutdownResult {
	d.closed.Store(true)
	var res ShutdownResult
	for _, s := range d.registry.Services() {
		if st, ok := s.(registry.Stopper); ok {
			canceled, running := st.Shutdown(ctx)
			res.CanceledTasks += canceled
			res.RunningTasks += running
		}
	}
	return res
}

func shutDown() *coreerr.Error {
	return coreerr.NewWithDetails(coreerr.CodeCanceled, "library is shut down", map[string]string{"reason": "shutdown"})
}
