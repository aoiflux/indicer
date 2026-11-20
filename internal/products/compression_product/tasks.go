package compression_product

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	coreerr "indicer/internal/core/errors"
	"indicer/internal/safe"
)

const (
	// maxActiveTasks bounds the async tasks running or waiting at once.
	maxActiveTasks = 4
	// maxRetainedTasks bounds the finished tasks kept for get_progress.
	maxRetainedTasks = 256
	// progressTaskTTL is how long a finished task is kept.
	progressTaskTTL = time.Hour
)

var (
	errShutdown        = errors.New("library is shut down")
	errCanceledRequest = errors.New("task canceled by request")
)

type taskFunc func(ctx context.Context, report progressReporter) (any, *coreerr.Error)

type progressReporter func(stage string, percent int, message string)

type progressRes struct {
	TaskID    string `json:"task_id"`
	Operation string `json:"operation"`
	Status    string `json:"status"`
	// Running is true until the task's work has returned; a cancelled task
	// can still be finishing.
	Running        bool           `json:"running"`
	Percent        int            `json:"percent"`
	Stage          string         `json:"stage,omitempty"`
	Message        string         `json:"message,omitempty"`
	Error          string         `json:"error,omitempty"`
	ErrorCode      string         `json:"error_code,omitempty"`
	ErrorDetails   map[string]any `json:"error_details,omitempty"`
	Result         any            `json:"result,omitempty"`
	StartedAtUTC   string         `json:"started_at_utc"`
	UpdatedAtUTC   string         `json:"updated_at_utc"`
	CompletedAtUTC string         `json:"completed_at_utc,omitempty"`
}

type asyncStartRes struct {
	TaskID string `json:"task_id"`
	Status string `json:"status"`
}

type cancelTaskRes struct {
	TaskID  string `json:"task_id"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

type progressTask struct {
	TaskID       string
	Operation    string
	Status       string
	Running      bool
	Percent      int
	Stage        string
	Message      string
	Error        string
	ErrorCode    string
	ErrorDetails map[string]any
	Result       any
	StartedAt    time.Time
	UpdatedAt    time.Time
	CompletedAt  time.Time
	ExpiresAt    time.Time
	cancel       context.CancelCauseFunc
}

// taskRunner runs a service's async tasks and keeps their progress. Tasks
// run on contexts derived from the runner's, so Shutdown reaches them all.
type taskRunner struct {
	mu     sync.Mutex
	seq    uint64
	tasks  map[string]*progressTask
	active int
	closed bool
	wg     sync.WaitGroup
	base   context.Context
	stop   context.CancelCauseFunc
}

func newTaskRunner() *taskRunner {
	base, stop := context.WithCancelCause(context.Background())
	return &taskRunner{tasks: make(map[string]*progressTask), base: base, stop: stop}
}

// start runs fn as a task. The task does not end with the request that
// started it, but keeps the request's deadline (timeout_ms).
func (r *taskRunner) start(ctx context.Context, operation string, fn taskFunc) (asyncStartRes, *coreerr.Error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return asyncStartRes{}, canceledFor(errShutdown)
	}
	now := time.Now().UTC()
	r.pruneLocked(now)
	if r.active >= maxActiveTasks {
		r.mu.Unlock()
		return asyncStartRes{}, coreerr.NewWithDetails(coreerr.CodeLimitExceeded, "too many async tasks", map[string]string{"limit": strconv.Itoa(maxActiveTasks)})
	}
	r.seq++
	id := fmt.Sprintf("cp-%d-%d", now.UnixNano(), r.seq)
	taskCtx, cancel := context.WithCancelCause(r.base)
	var stopDeadline context.CancelFunc = func() {}
	if dl, ok := ctx.Deadline(); ok {
		taskCtx, stopDeadline = context.WithDeadline(taskCtx, dl)
	}
	t := &progressTask{
		TaskID:    id,
		Operation: operation,
		Status:    statusQueued,
		Running:   true,
		StartedAt: now,
		UpdatedAt: now,
		ExpiresAt: now.Add(progressTaskTTL),
		cancel:    cancel,
	}
	r.tasks[id] = t
	r.active++
	r.wg.Add(1)
	r.mu.Unlock()

	go func() {
		defer r.wg.Done()
		defer cancel(nil)
		defer stopDeadline()
		r.update(id, statusRunning, 1, "starting", "operation started")
		report := func(stage string, percent int, message string) {
			r.update(id, statusRunning, percent, stage, message)
		}
		var rerr *coreerr.Error
		result, perr := safe.Value(safe.Site{Component: "qwikpak/task", Op: operation}, func() (any, error) {
			res, e := fn(taskCtx, report)
			rerr = e
			return res, nil
		})
		if perr != nil {
			rerr = coreerr.FromPanic(perr)
		}
		r.finish(id, taskCtx, result, rerr)
	}()

	return asyncStartRes{TaskID: id, Status: statusQueued}, nil
}

func (r *taskRunner) get(id string) (progressRes, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked(time.Now().UTC())
	t, ok := r.tasks[id]
	if !ok {
		return progressRes{}, false
	}
	res := progressRes{
		TaskID:       t.TaskID,
		Operation:    t.Operation,
		Status:       t.Status,
		Running:      t.Running,
		Percent:      t.Percent,
		Stage:        t.Stage,
		Message:      t.Message,
		Error:        t.Error,
		ErrorCode:    t.ErrorCode,
		ErrorDetails: t.ErrorDetails,
		Result:       t.Result,
		StartedAtUTC: t.StartedAt.Format(time.RFC3339),
		UpdatedAtUTC: t.UpdatedAt.Format(time.RFC3339),
	}
	if !t.CompletedAt.IsZero() {
		res.CompletedAtUTC = t.CompletedAt.Format(time.RFC3339)
	}
	return res, true
}

// count returns how many tasks are kept.
func (r *taskRunner) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.tasks)
}

func terminal(status string) bool {
	return status == statusCompleted || status == statusFailed || status == statusCanceled
}

func (r *taskRunner) update(id, status string, percent int, stage, message string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.tasks[id]
	if !ok || terminal(t.Status) {
		return
	}
	percent = max(percent, t.Percent)
	percent = min(max(percent, 0), 100)
	t.Status = status
	t.Percent = percent
	t.Stage = stage
	t.Message = message
	t.UpdatedAt = time.Now().UTC()
}

// finish records how a task's work returned and frees its slot.
func (r *taskRunner) finish(id string, ctx context.Context, result any, rerr *coreerr.Error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.active--
	t, ok := r.tasks[id]
	if !ok {
		return
	}
	now := time.Now().UTC()
	t.Running = false
	t.cancel = nil
	t.UpdatedAt = now
	t.ExpiresAt = now.Add(progressTaskTTL)
	if terminal(t.Status) {
		return // cancelled while finishing
	}
	t.CompletedAt = now
	switch {
	case rerr == nil:
		t.Status = statusCompleted
		t.Percent = 100
		t.Stage = "completed"
		t.Message = "operation completed"
		t.Result = result
	case ctx.Err() != nil || rerr.Code == coreerr.CodeCanceled:
		r.markCanceledLocked(t, canceledError(ctx), now)
	default:
		t.Status = statusFailed
		t.Percent = min(t.Percent, 99)
		t.Stage = "failed"
		t.Message = "operation failed"
		t.Error = rerr.Message
		t.ErrorCode = string(rerr.Code)
		t.ErrorDetails = make(map[string]any, len(rerr.Details))
		for k, v := range rerr.Details {
			t.ErrorDetails[k] = v
		}
	}
}

func (r *taskRunner) markCanceledLocked(t *progressTask, why *coreerr.Error, now time.Time) {
	t.Status = statusCanceled
	t.Stage = "canceled"
	t.Message = "operation canceled"
	t.Error = errTaskCanceled
	t.ErrorCode = string(coreerr.CodeCanceled)
	t.ErrorDetails = map[string]any{"reason": why.Details["reason"]}
	t.UpdatedAt = now
	t.CompletedAt = now
	t.ExpiresAt = now.Add(progressTaskTTL)
}

func (r *taskRunner) cancel(id string) (cancelTaskRes, *coreerr.Error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.tasks[id]
	if !ok {
		return cancelTaskRes{}, coreerr.New(coreerr.CodeInvalidRequest, errProgressTaskNotFound)
	}
	if terminal(t.Status) {
		return cancelTaskRes{TaskID: id, Status: t.Status, Message: errTaskNotCancelable}, nil
	}
	if t.cancel != nil {
		t.cancel(errCanceledRequest)
	}
	r.markCanceledLocked(t, canceledFor(errCanceledRequest), time.Now().UTC())
	return cancelTaskRes{TaskID: id, Status: statusCanceled}, nil
}

// pruneLocked drops expired finished tasks, then the oldest finished ones
// beyond maxRetainedTasks. Tasks still running are always kept.
func (r *taskRunner) pruneLocked(now time.Time) {
	var finished []*progressTask
	for id, t := range r.tasks {
		if t.Running {
			continue
		}
		if now.After(t.ExpiresAt) {
			delete(r.tasks, id)
			continue
		}
		finished = append(finished, t)
	}
	if extra := len(finished) - (maxRetainedTasks - maxActiveTasks); extra > 0 {
		sort.Slice(finished, func(i, j int) bool { return finished[i].UpdatedAt.Before(finished[j].UpdatedAt) })
		for _, t := range finished[:extra] {
			delete(r.tasks, t.TaskID)
		}
	}
}

// Shutdown cancels every task, refuses new ones and waits for the running
// ones until ctx ends.
func (r *taskRunner) Shutdown(ctx context.Context) (canceled, running int) {
	r.mu.Lock()
	r.closed = true
	now := time.Now().UTC()
	for _, t := range r.tasks {
		if !terminal(t.Status) {
			canceled++
			r.markCanceledLocked(t, canceledFor(errShutdown), now)
		}
	}
	r.mu.Unlock()
	r.stop(errShutdown)

	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return canceled, r.active
}

// canceledError describes why ctx ended: reason "shutdown", "timeout" or
// "canceled".
func canceledError(ctx context.Context) *coreerr.Error {
	if ctx == nil || ctx.Err() == nil {
		return canceledFor(errCanceledRequest)
	}
	return canceledFor(context.Cause(ctx))
}

func canceledFor(cause error) *coreerr.Error {
	reason := "canceled"
	switch {
	case errors.Is(cause, errShutdown):
		reason = "shutdown"
	case errors.Is(cause, context.DeadlineExceeded):
		reason = "timeout"
	}
	return coreerr.NewWithDetails(coreerr.CodeCanceled, errTaskCanceled, map[string]string{"reason": reason})
}
