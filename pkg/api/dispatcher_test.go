package api

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	coreerr "indicer/internal/core/errors"
	"indicer/internal/core/jsonbridge"
	"indicer/internal/products/registry"
)

// fakeService runs fn for every operation.
type fakeService struct {
	fn func(ctx context.Context, op string) (any, *coreerr.Error)
}

func (f fakeService) Product() string { return "fake" }

func (f fakeService) Handle(ctx context.Context, op string, _ json.RawMessage) (any, *coreerr.Error) {
	return f.fn(ctx, op)
}

func fakeDispatcher(fn func(ctx context.Context, op string) (any, *coreerr.Error)) *Dispatcher {
	r := registry.New()
	r.Register(fakeService{fn: fn})
	return NewDispatcher(r)
}

type envelope struct {
	Version string          `json:"version"`
	Status  string          `json:"status"`
	Error   *coreerr.Error  `json:"error"`
	Result  json.RawMessage `json:"result"`
}

func decode(t *testing.T, out string) envelope {
	t.Helper()
	if out == "" {
		t.Fatal("dispatch returned an empty string")
	}
	var env envelope
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("dispatch returned invalid JSON %q: %v", out, err)
	}
	return env
}

func request(op string, extra string) string {
	return `{"version":"v1","product":"fake","operation":"` + op + `","params":{}` + extra + `}`
}

func TestDispatchContainsPanics(t *testing.T) {
	t.Parallel()
	d := fakeDispatcher(func(context.Context, string) (any, *coreerr.Error) {
		panic("service panicked")
	})
	env := decode(t, d.DispatchJSON(context.Background(), request("x", "")))
	if env.Status != "error" || env.Error == nil || env.Error.Code != coreerr.CodeInternal {
		t.Fatalf("got %+v, want an internal_error", env)
	}
	if env.Error.Details["panic_id"] == "" {
		t.Fatalf("internal error carries no panic_id: %+v", env.Error)
	}
	if strings.Contains(env.Error.Message, "service panicked") {
		t.Fatalf("panic value leaked to the host: %q", env.Error.Message)
	}
}

func TestDispatchResultThatCannotBeEncoded(t *testing.T) {
	t.Parallel()
	d := fakeDispatcher(func(context.Context, string) (any, *coreerr.Error) {
		return map[string]float64{"x": math.NaN()}, nil
	})
	env := decode(t, d.DispatchJSON(context.Background(), request("x", "")))
	if env.Status != "error" || env.Error == nil || env.Error.Code != coreerr.CodeInternal {
		t.Fatalf("got %+v, want an internal_error", env)
	}
}

func TestDispatchRefusesOversizedRequests(t *testing.T) {
	t.Parallel()
	called := false
	d := fakeDispatcher(func(context.Context, string) (any, *coreerr.Error) {
		called = true
		return "ok", nil
	})
	big := `{"version":"v1","product":"fake","operation":"x","params":{"data":"` +
		strings.Repeat("A", jsonbridge.MaxRequestBytes) + `"}}`
	env := decode(t, d.DispatchJSON(context.Background(), big))
	if env.Error == nil || env.Error.Code != coreerr.CodeLimitExceeded {
		t.Fatalf("got %+v, want limit_exceeded", env)
	}
	if called {
		t.Fatal("an oversized request reached the service")
	}
}

func TestDispatchTimeout(t *testing.T) {
	t.Parallel()
	d := fakeDispatcher(func(ctx context.Context, _ string) (any, *coreerr.Error) {
		select {
		case <-ctx.Done():
			return nil, coreerr.FromContext(ctx)
		case <-time.After(10 * time.Second):
			return "finished", nil
		}
	})
	start := time.Now()
	env := decode(t, d.DispatchJSON(context.Background(), request("x", `,"timeout_ms":50`)))
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("timeout_ms 50 returned after %v", took)
	}
	if env.Error == nil || env.Error.Code != coreerr.CodeCanceled || env.Error.Details["reason"] != "timeout" {
		t.Fatalf("got %+v, want canceled with reason timeout", env)
	}
}

func TestDispatchHonoursCancellation(t *testing.T) {
	t.Parallel()
	d := fakeDispatcher(func(ctx context.Context, _ string) (any, *coreerr.Error) {
		<-ctx.Done()
		return nil, coreerr.FromContext(ctx)
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	env := decode(t, d.DispatchJSON(ctx, request("x", "")))
	if env.Error == nil || env.Error.Code != coreerr.CodeCanceled {
		t.Fatalf("got %+v, want canceled", env)
	}
}

func TestDispatchRejectsNegativeTimeout(t *testing.T) {
	t.Parallel()
	d := fakeDispatcher(func(context.Context, string) (any, *coreerr.Error) { return "ok", nil })
	env := decode(t, d.DispatchJSON(context.Background(), request("x", `,"timeout_ms":-1`)))
	if env.Error == nil || env.Error.Code != coreerr.CodeInvalidRequest {
		t.Fatalf("got %+v, want invalid_request", env)
	}
}

// TestDurationOfOperations: a response says how long its operation ran,
// whether it succeeded or failed; a request refused before running has no
// duration.
func TestDurationOfOperations(t *testing.T) {
	t.Parallel()
	d := fakeDispatcher(func(_ context.Context, op string) (any, *coreerr.Error) {
		time.Sleep(20 * time.Millisecond)
		if op == "fail" {
			return nil, coreerr.New(coreerr.CodeInvalidRequest, "no")
		}
		return map[string]int{"n": 1}, nil
	})
	duration := func(out string) (float64, bool) {
		var env map[string]any
		if err := json.Unmarshal([]byte(out), &env); err != nil {
			t.Fatal(err)
		}
		ms, ok := env["duration_ms"].(float64)
		return ms, ok
	}
	for _, op := range []string{"ok", "fail"} {
		out := d.DispatchJSON(t.Context(), `{"version":"v1","product":"fake","operation":"`+op+`"}`)
		if ms, ok := duration(out); !ok || ms < 20 {
			t.Errorf("%s: duration_ms %v (present %v), want at least 20 in %s", op, ms, ok, out)
		}
	}
	for _, req := range []string{`{`, `{"version":"v1","product":"none","operation":"x"}`} {
		if _, ok := duration(d.DispatchJSON(t.Context(), req)); ok {
			t.Errorf("refused request %s has a duration", req)
		}
	}
}
