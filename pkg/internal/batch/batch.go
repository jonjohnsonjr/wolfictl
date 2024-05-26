package batch

import (
	"context"
	"sync"
)

type Work[T any] struct {
	mutexec sync.Mutex
	f       func(context.Context, []T) error

	qutex  sync.Mutex
	queue  []T
	errchs []chan error
	once   *sync.Once
}

// New returns a [Work] that calls f with a slice of inputs passed to [Do],
// ensuring only one call to f() happens at a time.
func New[T any](f func(context.Context, []T) error) *Work[T] {
	return &Work[T]{
		f:      f,
		queue:  []T{},
		errchs: []chan error{},
		once:   &sync.Once{},
	}
}

// Do calls the Work function with the given task T and returns an error if it fails.
// Do is safe to call concurrently.
func (w *Work[T]) Do(ctx context.Context, task T) error {
	// Safely append task to queue.
	w.qutex.Lock()
	errch := make(chan error, 1) // We don't want to block.
	w.queue = append(w.queue, task)
	w.errchs = append(w.errchs, errch)
	once := w.once // Since w.once updates itself, hold a reference to the current one.
	w.qutex.Unlock()

	// If there is an in-flight f(), wait for it to finish.
	// Otherwise, we will wait on errch until another task arrives (if ever).
	// We want to immediately invoke f() after the previous batch finishes for any pending tasks.
	w.mutexec.Lock()
	defer w.mutexec.Unlock()

	// All callers will race to call this as soon as there is not an f() in flight.
	once.Do(func() {
		// Stop accepting new tasks.
		w.qutex.Lock()

		// Copy the queue and errchs to use for this batch.
		todo := w.queue
		errchs := w.errchs
		w.queue = make([]T, 0, len(todo))
		w.errchs = make([]chan error, 0, len(errchs))

		// TODO: How to make this safe?
		w.once = &sync.Once{}

		// Allow new tasks accumulate while we execute this batch.
		w.qutex.Unlock()

		// Actually execute the batch.
		err := w.f(ctx, todo)

		// Yield the result to all waiting callers.
		for _, ch := range errchs {
			ch <- err
		}
	})

	return <-errch
}
