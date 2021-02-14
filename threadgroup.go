// Package threadgroup provides a utility for performing organized clean
// shutdown and quick shutdown of long running groups of threads, such as
// networking threads, background threads, or resources like databases.
//
// The OnStop and AfterStop functions are helpers which enable shutdown code to
// be inlined with resource allocation, similar to defer. The difference is that
// `OnStop` and `AfterStop` will be called following tg.Stop, instead of when
// the parent function goes out of scope.
package threadgroup

import (
	"context"
	"sync"
	"time"

	"github.com/uplo-tech/errors"
)

// ErrStopped is returned by ThreadGroup methods if Stop has already been
// called.
var ErrStopped = errors.New("ThreadGroup already stopped")

// A ThreadGroup is a one-time-use object to manage the life cycle of a group
// of threads. It is a sync.WaitGroup that provides functions for coordinating
// actions and shutting down threads. After Stop() is called, the thread group
// is no longer useful.
//
// It is safe to call Add(), Done(), and Stop() concurrently.
type ThreadGroup struct {
	onStopFns    []func() error
	afterStopFns []func() error

	afterFnTimers   map[uint64]*time.Timer
	afterFnsCounter uint64

	once          sync.Once
	stopCtx       context.Context
	stopCtxCancel context.CancelFunc
	bmu           sync.Mutex // Protects 'Add' and 'Wait'.
	mu            sync.Mutex // Protects the 'onStopFns' and 'afterStopFns' variable
	wg            sync.WaitGroup
}

// init creates the stop channel for the thread group.
func (tg *ThreadGroup) init() {
	tg.stopCtx, tg.stopCtxCancel = context.WithCancel(context.Background())
	tg.afterFnTimers = make(map[uint64]*time.Timer)
}

// isStopped will return true if Stop() has been called on the thread group.
func (tg *ThreadGroup) isStopped() bool {
	tg.once.Do(tg.init)
	select {
	case <-tg.stopCtx.Done():
		return true
	default:
		return false
	}
}

// Add increments the thread group counter.
func (tg *ThreadGroup) Add() error {
	tg.bmu.Lock()
	defer tg.bmu.Unlock()

	if tg.isStopped() {
		return ErrStopped
	}
	tg.wg.Add(1)
	return nil
}

// AfterFunc works like time.AfterFunc, except that it will automatically cancel
// the function if tg.Stop() is called. The thread group uses timers and
// time.AfterFunc for maximal memory safety and efficiency.
func (tg *ThreadGroup) AfterFunc(duration time.Duration, fn func()) {
	// The entire afterFns object needs to be locked for the rest of the
	// function, if afterFn is executed it needs to be blocked until the timer
	// has been added to the map.
	tg.bmu.Lock()
	defer tg.bmu.Unlock()

	// isStopped will run init().
	if tg.isStopped() {
		// Do not run the function if we are already stopped.
		return
	}

	// Grab an id for this function.
	id := tg.afterFnsCounter
	tg.afterFnsCounter++

	// Wrap the supplied function with a function that removes the timer from
	// the map.
	afterFn := func() {
		// afterFn will be run by the timer in a brand new goroutine if the
		// timer fires. But afterFn will not be run at all if the timer does not
		// fire. Because afterFn may not run at all, we need to call tg.Add
		// inside of afterFn instead of outside of it.
		//
		// We also use tg.Add to check if the threadgroup is stopped. There is a
		// tiny race between stopping the threadgroup and stopping all of the
		// timers, which means a few extra functions may slip through. This call
		// to tg.Add() will catch those functions and ensure they only run if
		// tg.Stop has not been called yet.
		if tg.Add() != nil {
			return
		}
		defer tg.Done()
		fn()
		tg.bmu.Lock()
		delete(tg.afterFnTimers, id)
		tg.bmu.Unlock()
	}

	// Create a timer to run the function.
	t := time.AfterFunc(duration, afterFn)
	tg.afterFnTimers[id] = t
}

// AfterStop ensures that a function will be called after Stop() has been called
// and after all running routines have called Done(). The functions will be
// called in reverse order to how they were added, similar to defer. If Stop()
// has already been called, the input function will not be run and an error will
// be returned.
//
// The primary use of AfterStop is to allow code that opens and closes resources
// to be positioned next to each other. The purpose is similar to `defer`,
// except for resources that outlive the function which creates them.
func (tg *ThreadGroup) AfterStop(fn func() error) error {
	tg.mu.Lock()
	if tg.isStopped() {
		tg.mu.Unlock()
		return ErrStopped
	}
	tg.afterStopFns = append(tg.afterStopFns, fn)
	tg.mu.Unlock()
	return nil
}

// Launch will launch a goroutine with the thread group.
//
// Correct use looks like this:
//
//	err := tg.Launch(fn)
//	if err != nil {
//		// handle err, fn was not executed.
//	}
//
// This is equivalent to writing:
//
//	err := tg.Add()
//	if err != nil {
//		return err
//	}
//	go func() {
//		fn()
//		tg.Done()
//	}
//	return nil
//
//  This is a convenience function which makes it more difficult to misuse
//  tg.Add and tg.Done when launching new threads.
func (tg *ThreadGroup) Launch(fn func()) error {
	err := tg.Add()
	if err != nil {
		return err
	}
	go func() {
		fn()
		tg.Done()
	}()
	return nil
}

// OnStop ensures that a function will be called after Stop() has been called,
// and before blocking until all running routines have called Done(). It is safe
// to use OnStop to coordinate the closing of long-running threads. The OnStop
// functions will be called in the reverse order in which they were added,
// similar to defer. If Stop() has already been called, the input function will
// not be called, and ErrStopped will be returned.
func (tg *ThreadGroup) OnStop(fn func() error) error {
	tg.mu.Lock()
	if tg.isStopped() {
		tg.mu.Unlock()
		return ErrStopped
	}
	tg.onStopFns = append(tg.onStopFns, fn)
	tg.mu.Unlock()
	return nil
}

// Done decrements the thread group counter.
func (tg *ThreadGroup) Done() {
	tg.wg.Done()
}

// Sleep will sleep for the provided duration. If the threadgroup is stopped
// before the duration is complete, Sleep will return early with the value
// 'false', indicating that shutdown has occurred. Otherwise Sleep will return
// after the given duration with a value of 'true'.
//
// Sleep uses a timer and cleans up correctly to ensure there is no time.After()
// related memory leak.
func (tg *ThreadGroup) Sleep(d time.Duration) bool {
	tg.once.Do(tg.init)

	// Do a quick check whether the thread group is already stopped.
	select {
	case <-tg.stopCtx.Done():
		return false
	default:
	}

	t := time.NewTimer(d)
	select {
	case <-t.C:
		return true
	case <-tg.stopCtx.Done():
	}

	// tg has been stopped, clean up the timer and return false.
	if !t.Stop() {
		<-t.C
	}
	return false
}

// Stop will close the stop channel of the thread group, then call all 'OnStop'
// functions in reverse order, then will wait until the thread group counter
// reaches zero, then will call all of the 'AfterStop' functions in reverse
// order.
//
// The errors returned by the OnStop and AfterStop functions will be composed
// into a single error.
func (tg *ThreadGroup) Stop() error {
	// Signal that the threadgroup is shutting down.
	if tg.isStopped() {
		return ErrStopped
	}
	tg.bmu.Lock()
	tg.stopCtxCancel()
	tg.bmu.Unlock()

	// Flush any function that made it past isStopped and might be trying to do
	// something under the mu lock. Any calls to OnStop or AfterStop after this
	// will fail, because isStopped will cut them short.
	tg.mu.Lock()
	tg.mu.Unlock()

	// Run all of the OnStop functions, in reverse order of how they were added.
	var err error
	for i := len(tg.onStopFns) - 1; i >= 0; i-- {
		err = errors.Extend(err, tg.onStopFns[i]())
	}

	// Wait for all running processes to signal completion.
	tg.wg.Wait()

	// Close out any AfterFnTimers that are still in the threadgroup. This has
	// to be done after the call to wg.Wait() to avoid a race condition on
	// tg.afterFnTimers.
	for _, t := range tg.afterFnTimers {
		t.Stop()
	}
	tg.afterFnTimers = nil

	// Run all of the AfterStop functions, in reverse order of how they were
	// added.
	for i := len(tg.afterStopFns) - 1; i >= 0; i-- {
		err = errors.Extend(err, tg.afterStopFns[i]())
	}
	return err
}

// StopChan provides read-only access to the ThreadGroup's stopChan. Callers
// should select on StopChan in order to interrupt long-running reads (such as
// time.After).
func (tg *ThreadGroup) StopChan() <-chan struct{} {
	tg.once.Do(tg.init)
	return tg.stopCtx.Done()
}

// StopCtx returns the threadgroups underlying context.
func (tg *ThreadGroup) StopCtx() context.Context {
	tg.once.Do(tg.init)
	return tg.stopCtx
}
