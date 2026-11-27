package core

import (
	"context"
	"errors"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"runtime"
	"time"
)

var ErrPanic = errors.New("plugin panic")
var ErrTimeout = errors.New("plugin timeout")
var ErrPlugin = errors.New("plugin failure")

// PluginError contains a plugin's error: errors.Is(err, ErrPlugin) holds, the
// original value is dropped (so it cannot impersonate core sentinels such as
// ErrTimeout), and only a redacted, bounded description of its cause is kept.
type PluginError struct{ Cause string }

func (e *PluginError) Error() string {
	if e.Cause == "" {
		return ErrPlugin.Error()
	}
	return ErrPlugin.Error() + ": " + e.Cause
}
func (e *PluginError) Is(target error) bool { return target == ErrPlugin }

// Contain converts an error returned by plugin code into a PluginError.
// Context cancellation passes through unchanged.
func Contain(e error) error {
	if e == nil || errors.Is(e, context.Canceled) || errors.Is(e, context.DeadlineExceeded) {
		return e
	}
	var pe *PluginError
	if errors.As(e, &pe) {
		return &PluginError{Cause: pluginapi.Redact(pe.Cause)}
	}
	return &PluginError{Cause: pluginapi.Redact(e.Error())}
}

// panicError keeps ErrPanic identity and a redacted description of the value.
type panicError struct{ cause string }

func (e *panicError) Error() string {
	if e.cause == "" {
		return ErrPanic.Error()
	}
	return ErrPanic.Error() + ": " + e.cause
}
func (e *panicError) Is(target error) bool { return target == ErrPanic }

// containPanic keeps the description only of runtime faults (nil dereference,
// index out of range); arbitrary panic values may carry plugin data and are dropped.
func containPanic(v any) error {
	if re, ok := v.(runtime.Error); ok {
		return &panicError{cause: pluginapi.Redact(re.Error())}
	}
	return &panicError{}
}

type result[T any] struct {
	v   T
	err error
}

// Call contains panics and bounds a noncooperative call. Callers quarantine on
// timeout, so an offending workspace can strand at most one call plus Stop.
func Call[T any](ctx context.Context, timeout time.Duration, fn func(context.Context) (T, error)) (T, error) {
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	callCtx := ctx
	cancel := func() {}
	if timeout > 0 {
		callCtx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()
	ch := make(chan result[T], 1)
	go func() {
		r := result[T]{}
		defer func() {
			if v := recover(); v != nil {
				r.err = containPanic(v)
			}
			ch <- r
		}()
		if r.err = callCtx.Err(); r.err != nil {
			if ctx.Err() == nil {
				r.err = ErrTimeout
			}
			return
		}
		r.v, r.err = fn(callCtx)
		r.err = Contain(r.err)
	}()
	select {
	case r := <-ch:
		return r.v, r.err
	case <-callCtx.Done():
		if ctx.Err() != nil {
			return zero, ctx.Err()
		}
		return zero, ErrTimeout
	}
}
func Guard(ctx context.Context, timeout time.Duration, fn func(context.Context) error) error {
	_, e := Call(ctx, timeout, func(c context.Context) (struct{}, error) { return struct{}{}, fn(c) })
	return e
}
