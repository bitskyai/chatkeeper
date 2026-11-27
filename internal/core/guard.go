package core

import (
	"context"
	"errors"
	"time"
)

var ErrPanic = errors.New("plugin panic")
var ErrTimeout = errors.New("plugin timeout")
var ErrPlugin = errors.New("plugin failure")

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
			if recover() != nil {
				r.err = ErrPanic
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
		if r.err != nil && !errors.Is(r.err, context.Canceled) && !errors.Is(r.err, context.DeadlineExceeded) {
			r.err = ErrPlugin
		}
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
