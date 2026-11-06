package netguard

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

var (
	ErrResponseIdle    = fmt.Errorf("response stopped making progress: %w", context.DeadlineExceeded)
	ErrRequestDeadline = fmt.Errorf("request time budget exceeded: %w", context.DeadlineExceeded)
)

type requestBudgets struct{ total, idle time.Duration }

func (guard *Guard) budgets(purpose Purpose) requestBudgets {
	total, idle := guard.policy.RequestTimeout, guard.policy.BodyIdleTimeout
	if total <= 0 {
		total = 10 * time.Minute
	}
	if idle <= 0 {
		idle = 2 * time.Minute
	}
	if purpose == LLM {
		total, idle = guard.policy.ProviderTimeout, guard.policy.ProviderIdleTimeout
		if total <= 0 {
			total = 30 * time.Minute
		}
		if idle <= 0 {
			idle = 10 * time.Minute
		}
	}
	return requestBudgets{total, idle}
}

func boundedRoundTrip(base http.RoundTripper, request *http.Request, purpose Purpose, budget requestBudgets) (*http.Response, error) {
	deadline, cancelDeadline := context.WithTimeoutCause(request.Context(), budget.total, ErrRequestDeadline)
	ctx, cancel := context.WithCancelCause(deadline)
	response, err := base.RoundTrip(request.Clone(ctx))
	if err != nil {
		cause := context.Cause(ctx)
		cancelDeadline()
		cancel(nil)
		if cause != nil {
			return nil, fmt.Errorf("%s request: %w", purpose, cause)
		}
		return nil, err
	}
	body := &progressBody{body: response.Body, ctx: ctx, purpose: purpose, idle: budget.idle, cancel: cancel, cancelDeadline: cancelDeadline}
	body.timer = time.AfterFunc(budget.idle, func() { cancel(ErrResponseIdle) })
	original := response.Body
	body.stop = context.AfterFunc(ctx, func() { _ = original.Close() })
	response.Body = body
	return response, nil
}

type progressBody struct {
	body           io.ReadCloser
	ctx            context.Context
	purpose        Purpose
	idle           time.Duration
	cancel         context.CancelCauseFunc
	cancelDeadline context.CancelFunc
	timer          *time.Timer
	stop           func() bool
	mutex          sync.Mutex
	done           bool
}

func (body *progressBody) release() {
	body.mutex.Lock()
	defer body.mutex.Unlock()
	if body.done {
		return
	}
	body.done = true
	body.timer.Stop()
	body.stop()
	body.cancelDeadline()
	body.cancel(nil)
}
func (body *progressBody) Read(buffer []byte) (int, error) {
	n, err := body.body.Read(buffer)
	cause := context.Cause(body.ctx)
	if cause != nil {
		err = fmt.Errorf("%s response: %w", body.purpose, cause)
	}
	if err != nil {
		body.release()
		return n, err
	}
	if n > 0 {
		body.mutex.Lock()
		if !body.done {
			body.timer.Reset(body.idle)
		}
		body.mutex.Unlock()
	}
	return n, nil
}
func (body *progressBody) Close() error { body.release(); return body.body.Close() }
