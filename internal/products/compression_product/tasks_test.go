package compression_product

import (
	"context"
	"errors"
	"testing"
	"time"

	coreerr "indicer/internal/core/errors"
)

func blockUntilDone(ctx context.Context, _ progressReporter) (any, *coreerr.Error) {
	<-ctx.Done()
	return nil, canceledError(ctx)
}

func startTask(t *testing.T, svc *Service, ctx context.Context, fn taskFunc) string {
	t.Helper()
	res, rerr := svc.startAsyncTask(ctx, "test_op", fn)
	if rerr != nil {
		t.Fatalf("startAsyncTask: %v %v", rerr, rerr.Details)
	}
	return res.(asyncStartRes).TaskID
}

// waitTask polls a task until it is in a terminal state.
func waitTask(t *testing.T, svc *Service, id string) progressRes {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		p, ok := svc.tasks.get(id)
		if !ok {
			t.Fatalf("task %s is gone", id)
		}
		if p.Status == statusCompleted || p.Status == statusFailed || (p.Status == statusCanceled && !p.Running) {
			return p
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("task %s did not finish", id)
	return progressRes{}
}

// TestAsyncTaskPanicIsContained is the FFI-1 regression for async tasks: a
// panic on a task's goroutine ended the host process.
func TestAsyncTaskPanicIsContained(t *testing.T) {
	svc := New()
	id := startTask(t, svc, context.Background(), func(context.Context, progressReporter) (any, *coreerr.Error) {
		panic("task panicked")
	})
	p := waitTask(t, svc, id)
	if p.Status != statusFailed || p.ErrorCode != string(coreerr.CodeInternal) || p.ErrorDetails["panic_id"] == nil {
		t.Fatalf("task after a panic: %+v", p)
	}
}

func TestAsyncTasksAreCapped(t *testing.T) {
	svc := New()
	var ids []string
	for i := 0; i < maxActiveTasks; i++ {
		ids = append(ids, startTask(t, svc, context.Background(), blockUntilDone))
	}
	if _, rerr := svc.startAsyncTask(context.Background(), "test_op", blockUntilDone); rerr == nil || rerr.Code != coreerr.CodeLimitExceeded {
		t.Fatalf("task %d: %v, want limit_exceeded", maxActiveTasks+1, rerr)
	}
	if _, rerr := svc.tasks.cancel(ids[0]); rerr != nil {
		t.Fatal(rerr)
	}
	waitTask(t, svc, ids[0])
	ids = append(ids, startTask(t, svc, context.Background(), blockUntilDone))
	for _, id := range ids[1:] {
		if _, rerr := svc.tasks.cancel(id); rerr != nil {
			t.Fatal(rerr)
		}
	}
	for _, id := range ids[1:] {
		if p := waitTask(t, svc, id); p.Status != statusCanceled || p.ErrorCode != string(coreerr.CodeCanceled) {
			t.Fatalf("cancelled task: %+v", p)
		}
	}
}

func TestAsyncTaskDeadline(t *testing.T) {
	svc := New()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	id := startTask(t, svc, ctx, blockUntilDone)
	p := waitTask(t, svc, id)
	if p.Status != statusCanceled || p.ErrorDetails["reason"] != "timeout" {
		t.Fatalf("task past its deadline: %+v", p)
	}
}

// TestAsyncTaskOutlivesItsRequest: an async request returns at once, and
// the end of its request's context does not stop the task.
func TestAsyncTaskOutlivesItsRequest(t *testing.T) {
	svc := New()
	ctx, cancel := context.WithCancel(context.Background())
	release := make(chan struct{})
	id := startTask(t, svc, ctx, func(ctx context.Context, _ progressReporter) (any, *coreerr.Error) {
		select {
		case <-release:
			return "done", nil
		case <-ctx.Done():
			return nil, canceledError(ctx)
		}
	})
	cancel()
	time.Sleep(20 * time.Millisecond)
	close(release)
	if p := waitTask(t, svc, id); p.Status != statusCompleted {
		t.Fatalf("task after its request ended: %+v", p)
	}
}

func TestShutdownCancelsTasks(t *testing.T) {
	svc := New()
	a := startTask(t, svc, context.Background(), blockUntilDone)
	b := startTask(t, svc, context.Background(), blockUntilDone)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	canceled, running := svc.Shutdown(ctx)
	if canceled != 2 || running != 0 {
		t.Fatalf("Shutdown = %d cancelled, %d running; want 2, 0", canceled, running)
	}
	for _, id := range []string{a, b} {
		if p, _ := svc.tasks.get(id); p.Status != statusCanceled || p.ErrorDetails["reason"] != "shutdown" {
			t.Fatalf("task after shutdown: %+v", p)
		}
	}
	if _, rerr := svc.startAsyncTask(context.Background(), "test_op", blockUntilDone); rerr == nil || rerr.Code != coreerr.CodeCanceled {
		t.Fatalf("task after shutdown: %v, want canceled", rerr)
	}
}

func TestShutdownReportsStuckTasks(t *testing.T) {
	svc := New()
	release := make(chan struct{})
	defer close(release)
	startTask(t, svc, context.Background(), func(context.Context, progressReporter) (any, *coreerr.Error) {
		<-release // ignores cancellation
		return nil, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if canceled, running := svc.Shutdown(ctx); canceled != 1 || running != 1 {
		t.Fatalf("Shutdown = %d cancelled, %d running; want 1, 1", canceled, running)
	}
}

func TestFinishedTasksAreBounded(t *testing.T) {
	svc := New()
	for i := 0; i < maxRetainedTasks+40; i++ {
		id := startTask(t, svc, context.Background(), func(context.Context, progressReporter) (any, *coreerr.Error) {
			return i, nil
		})
		waitTask(t, svc, id)
	}
	if n := svc.tasks.count(); n > maxRetainedTasks {
		t.Fatalf("%d finished tasks kept, want at most %d", n, maxRetainedTasks)
	}
}

func TestCanceledErrorReasons(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(errShutdown)
	if r := canceledError(ctx).Details["reason"]; r != "shutdown" {
		t.Fatalf("reason %q", r)
	}
	ctx, cancel = context.WithCancelCause(context.Background())
	cancel(errors.New("anything"))
	if r := canceledError(ctx).Details["reason"]; r != "canceled" {
		t.Fatalf("reason %q", r)
	}
}
