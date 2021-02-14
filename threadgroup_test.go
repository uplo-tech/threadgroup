package threadgroup

import (
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestConcurrency is a high concurrency test that creates a bunch of thread
// groups, runs a bunch of concurrent operations on them, and then closes
// everything out while still running lots of concurrent operations.
//
// The goal of this test is to make sure the race detector doesn't fire, that no
// deadlocks are hit, and that the basic business logic is still correct.
//
// Functions covered:
//	+ Add()
//	+ Done()
//	+ Sleep()
//
//	+ Launch()
//
//	+ StopChan()
//
//	+ OnStop()
//	+ AfterStop()
//
//	+ AfterFunc()
//
//	+ Stop()
func TestConcurrency(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}

	// Create a function to test Add(), Done(), and Sleep() concurrency safety.
	addDoneSleep := func(tg *ThreadGroup) {
		// Defer a function to call tg.Add and tg.Sleep after the threadgroup is
		// closed.
		defer func() {
			if tg.Add() == nil {
				t.Error("tg should not accept an Add call after closing")
			}
			if tg.Sleep(1) {
				t.Error("tg should not accept a Sleep call after closing")
			}
		}()
		for {
			// Do a triple Add.
			if tg.Add() != nil {
				return
			}
			if tg.Add() != nil {
				tg.Done()
				return
			}
			if tg.Add() != nil {
				tg.Done()
				tg.Done()
				return
			}
			tg.Done()
			tg.Done()
			tg.Done()
			if !tg.Sleep(time.Millisecond) {
				return
			}
		}
	}

	// Create a function to test Launch() safety.
	launch := func(tg *ThreadGroup) {
		// LaunchFn needs something to do. Have it atomically increment a
		// counter so that the pressure is as heavy as possible on launching
		// more things.
		var atomicCounter uint64
		launchFn := func() {
			atomic.AddUint64(&atomicCounter, 1)
		}

		// Defer a function to do a launch after the tg has closed.
		defer func() {
			if tg.Launch(launchFn) == nil {
				t.Error("tg should not accept a Launch call after closing")
			}
		}()
		for {
			// Launch 15 threads at once.
			for i := 0; i < 15; i++ {
				if tg.Launch(launchFn) != nil {
					return
				}
			}
			if !tg.Sleep(time.Millisecond) {
				return
			}
		}
	}

	// stopChan will check safety around the StopChan() function.
	stopChan := func(tg *ThreadGroup) {
		// Defer a function to do a StopChan call after the tg has closed.
		defer func() {
			<-tg.StopChan()
		}()
		for {
			// Launch 10 threads at a time to wait for the StopChan signal.
			for i := 0; i < 10; i++ {
				go func() {
					if tg.Add() != nil {
						return
					}
					<-tg.StopChan()
					tg.Done()
				}()
			}
			if !tg.Sleep(time.Millisecond) {
				return
			}
		}
	}

	// onAfterStop checks the concurrency around the OnStop and AfterStop calls,
	// also ensures OnStop funcs all run before AfterStop funcs.
	onAfterStop := func(tg *ThreadGroup) {
		// afterStopped should not need any thread safety around it, because the
		// threadgroup does not do OnStop and AfterStop in parallel.
		var afterStopped uint64
		onStopFn := func() error {
			// Check that afterStopped is still zero.
			if afterStopped != 0 {
				t.Error("OnStop running after AfterStop")
			}
			return nil
		}
		afterStopFn := func() error {
			// Increment afterStopped.
			afterStopped++
			return nil
		}

		// Defer a function to ensure that OnStop and AfterStop fail if called
		// after the tg is closed.
		defer func() {
			if tg.OnStop(onStopFn) == nil {
				t.Error("OnStop should not run after the thread group has stopped")
			}
			if tg.AfterStop(afterStopFn) == nil {
				t.Error("AfterStop should error after the thread group has stopped")
			}
		}()
		for {
			// Create a bunch of OnStop and AfterStop functions.
			if tg.OnStop(onStopFn) != nil {
				return
			}
			if tg.AfterStop(afterStopFn) != nil {
				return
			}
			if !tg.Sleep(time.Millisecond) {
				return
			}
		}
	}

	afterFunc := func(tg *ThreadGroup) {
		// Create a busywork function to run after some period of time.
		var atomicAfterFunc uint64
		afterFn := func() {
			atomic.AddUint64(&atomicAfterFunc, 1)
		}
		// Defer a function to ensure that AfterFunc fails once the tg is
		// closed.
		defer func() {
			latest := atomic.LoadUint64(&atomicAfterFunc)
			tg.AfterFunc(1, afterFn)
			time.Sleep(time.Millisecond * 20)
			if latest != atomic.LoadUint64(&atomicAfterFunc) {
				t.Error("seems like afterFuncs are still running after tg has closed")
			}
		}()
		for {
			for i := 0; i < 8; i++ {
				tg.AfterFunc(time.Millisecond, afterFn)
			}
			if !tg.Sleep(time.Millisecond) {
				return
			}
		}
	}

	// Run the whole test 200 times.
	for i := 0; i < 200; i++ {
		tg := new(ThreadGroup)

		// Launch a bunch of threads. Use a WaitGroup to ensure they all exit.
		var wg sync.WaitGroup
		wg.Add(5)
		go func() {
			addDoneSleep(tg)
			wg.Done()
		}()
		go func() {
			launch(tg)
			wg.Done()
		}()
		go func() {
			stopChan(tg)
			wg.Done()
		}()
		go func() {
			onAfterStop(tg)
			wg.Done()
		}()
		go func() {
			afterFunc(tg)
			wg.Done()
		}()

		// Give all of the threads some time to rapid cycle. Each iteration
		// gives a different amount of time to maximize the surface area that we
		// cover.
		time.Sleep(time.Millisecond * time.Duration(i))
		// Kill all of the threads.
		err := tg.Stop()
		if err != nil {
			t.Fatal(err)
		}

		// Wait for all of the background threads to clean up fully.
		wg.Wait()
	}
}

// TestAfterFunc checks that the AfterFunc method of the tg is working as
// advertised.
func TestAfterFunc(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}

	// signal is the variable we are using to check AfterFunc. mu protects
	// signal.
	var mu sync.Mutex
	signal := 0

	// Try basic, standard use of AfterFunc.
	duration := 3 * time.Second
	var tg ThreadGroup
	tg.AfterFunc(duration, func() {
		mu.Lock()
		signal++
		mu.Unlock()
	})
	if len(tg.afterFnTimers) != 1 {
		t.Error("tg.afterFnTimers should have a timer in it")
	}
	time.Sleep(duration / 3)
	mu.Lock()
	s := signal
	mu.Unlock()
	if s != 0 {
		t.Error("AfterFunc fired too early")
	}
	time.Sleep(duration)
	mu.Lock()
	s = signal
	mu.Unlock()
	if s != 1 {
		t.Error("AfterFunc did not fire")
	}
	tg.bmu.Lock()
	afLen := len(tg.afterFnTimers)
	tg.bmu.Unlock()
	if afLen != 0 {
		t.Error("tg.afterFnTimers is not cleaned up")
	}

	// Queue up a bunch of afterFns to run simultaneously. Use powers of 2 so
	// that the results can't overlap.
	tg.AfterFunc(duration/5, func() {
		mu.Lock()
		signal += 2
		mu.Unlock()
	})
	tg.AfterFunc(duration/4, func() {
		mu.Lock()
		signal += 4
		mu.Unlock()
	})
	tg.AfterFunc(duration/3, func() {
		mu.Lock()
		signal += 8
		mu.Unlock()
	})
	tg.AfterFunc(duration/2, func() {
		mu.Lock()
		signal += 16
		mu.Unlock()
	})
	time.Sleep(duration)
	mu.Lock()
	s = signal
	mu.Unlock()
	if s != 31 {
		t.Error("not all afterFns fired", s)
	}
	tg.bmu.Lock()
	afLen = len(tg.afterFnTimers)
	tg.bmu.Unlock()
	if afLen != 0 {
		t.Error("afterFnTimers not cleaned up correctly", len(tg.afterFnTimers))
	}

	// Interrupt the afterFns.
	tg.AfterFunc(duration, func() {
		mu.Lock()
		signal += 32
		mu.Unlock()
	})
	tg.AfterFunc(duration, func() {
		mu.Lock()
		signal += 64
		mu.Unlock()
	})
	tg.AfterFunc(duration, func() {
		mu.Lock()
		signal += 128
		mu.Unlock()
	})

	// Try a series of AfterFuncs that fire immediately.
	tg.AfterFunc(0, func() {
		mu.Lock()
		signal += 256
		mu.Unlock()
	})
	tg.AfterFunc(0, func() {
		mu.Lock()
		signal += 512
		mu.Unlock()
	})
	tg.AfterFunc(-1, func() {
		mu.Lock()
		signal += 1024
		mu.Unlock()
	})
	time.Sleep(time.Millisecond * 50)
	mu.Lock()
	s = signal
	mu.Unlock()
	if s != 1823 {
		t.Error("afterFns should have fired immediately", s)
	}

	// Try firing some afterFns after Stop() is called.
	err := tg.Stop()
	if err != nil {
		t.Fatal(err)
	}
	tg.bmu.Lock()
	afLen = len(tg.afterFnTimers)
	tg.bmu.Unlock()
	if afLen != 0 {
		t.Error("afterFns not cleaned up correctly")
	}
	mu.Lock()
	s = signal
	mu.Unlock()
	if s != 1823 {
		t.Error("afterFns should not have fired")
	}

	// Try queing a function after stop has been called.
	tg.AfterFunc(duration, func() {
		mu.Lock()
		signal += 2048
		mu.Unlock()
	})
	tg.bmu.Lock()
	afLen = len(tg.afterFnTimers)
	tg.bmu.Unlock()
	if afLen != 0 {
		t.Error("afterFns not being cleaned up correctly")
	}
	mu.Lock()
	s = signal
	mu.Unlock()
	if s != 1823 {
		t.Error("afterFns should not have fired")
	}
	time.Sleep(duration)
	mu.Lock()
	s = signal
	mu.Unlock()
	if s != 1823 {
		t.Error("afterFns should not have fired")
	}
}

// TestThreadGroupStopEarly tests that a thread group can correctly interrupt
// an ongoing process.
func TestThreadGroupStopEarly(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}

	var tg ThreadGroup
	for i := 0; i < 10; i++ {
		err := tg.Add()
		if err != nil {
			t.Fatal(err)
		}

		go func() {
			defer tg.Done()
			select {
			case <-time.After(1 * time.Second):
			case <-tg.StopChan():
			}
		}()
	}
	start := time.Now()
	err := tg.Stop()
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	} else if elapsed > 500*time.Millisecond {
		t.Fatal("Stop did not interrupt goroutines")
	}
}

// TestThreadGroupWait tests that a thread group will correctly wait for
// existing processes to halt.
func TestThreadGroupWait(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}

	var tg ThreadGroup
	for i := 0; i < 10; i++ {
		err := tg.Add()
		if err != nil {
			t.Fatal(err)
		}

		go func() {
			defer tg.Done()
			time.Sleep(time.Second)
		}()
	}
	start := time.Now()
	err := tg.Stop()
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	} else if elapsed < time.Millisecond*950 {
		t.Fatal("Stop did not wait for goroutines:", elapsed)
	}
}

// TestThreadGroupSleep tests that the thread group sleep function works
// correctly.
func TestThreadGroupSleep(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}

	// Try a basic sleep.
	start := time.Now()
	var tg ThreadGroup
	sleepDuration := time.Second * 3
	if !tg.Sleep(sleepDuration) {
		t.Error("sleep should have finished")
	}
	if time.Since(start) < sleepDuration {
		t.Error("sleep did not last long enough")
	}

	// Try interrupting a sleep.
	start = time.Now()
	go func() {
		time.Sleep(sleepDuration / 3)
		err := tg.Stop()
		if err != nil {
			t.Error(err)
		}
	}()
	if tg.Sleep(sleepDuration) {
		t.Error("sleep should have been interrupted")
	}
	if time.Since(start) > sleepDuration/2 {
		t.Error("sleep lasted too long")
	}
}

// TestThreadGroupStop tests the behavior of a ThreadGroup after Stop has been
// called.
func TestThreadGroupStop(t *testing.T) {
	// Create a thread group and stop it.
	var tg ThreadGroup
	// Create an array to track the order of execution for OnStop and AfterStop
	// calls.
	var stopCalls []int

	// isStopped should return false
	if tg.isStopped() {
		t.Error("isStopped returns true on unstopped ThreadGroup")
	}
	// The cannel provided by StopChan should be open.
	select {
	case <-tg.StopChan():
		t.Error("stop chan appears to be closed")
	default:
	}

	// OnStop and AfterStop should queue their functions, but not call them.
	// 'Add' and 'Done' are setup around the OnStop functions, to make sure
	// that the OnStop functions are called before waiting for all calls to
	// 'Done' to come through.
	//
	// Note: the practice of calling Add outside of OnStop and Done inside of
	// OnStop is a bad one - any call to tg.Flush() will cause a deadlock
	// because the stop functions will not be called but tg.Flush will be
	// waiting for the thread group counter to reach zero.
	err := tg.Add()
	if err != nil {
		t.Fatal(err)
	}
	err = tg.Add()
	if err != nil {
		t.Fatal(err)
	}
	err = tg.OnStop(func() error {
		tg.Done()
		stopCalls = append(stopCalls, 1)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	err = tg.OnStop(func() error {
		tg.Done()
		stopCalls = append(stopCalls, 2)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	err = tg.AfterStop(func() error {
		stopCalls = append(stopCalls, 10)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	err = tg.AfterStop(func() error {
		stopCalls = append(stopCalls, 20)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// None of the stop calls should have been called yet.
	if len(stopCalls) != 0 {
		t.Fatal("Stop calls were called too early")
	}

	// Stop the thread group.
	err = tg.Stop()
	if err != nil {
		t.Fatal(err)
	}
	// isStopped should return true.
	if !tg.isStopped() {
		t.Error("isStopped returns false on stopped ThreadGroup")
	}
	// The cannel provided by StopChan should be closed.
	select {
	case <-tg.StopChan():
	default:
		t.Error("stop chan appears to be closed")
	}
	// The OnStop calls should have been called first, in reverse order, and
	// the AfterStop calls should have been called second, in reverse order.
	if len(stopCalls) != 4 {
		t.Fatal("Stop did not call the stopping functions correctly")
	}
	if stopCalls[0] != 2 {
		t.Error("Stop called the stopping functions in the wrong order")
	}
	if stopCalls[1] != 1 {
		t.Error("Stop called the stopping functions in the wrong order")
	}
	if stopCalls[2] != 20 {
		t.Error("Stop called the stopping functions in the wrong order")
	}
	if stopCalls[3] != 10 {
		t.Error("Stop called the stopping functions in the wrong order")
	}

	// Add and Stop should return errors.
	err = tg.Add()
	if err != ErrStopped {
		t.Error("expected ErrStopped, got", err)
	}
	err = tg.Stop()
	if err != ErrStopped {
		t.Error("expected ErrStopped, got", err)
	}

	// OnStop and AfterStop should not call their functions now that the thread
	// group has stopped.
	onStopCalled := false
	err = tg.OnStop(func() error {
		onStopCalled = true
		return nil
	})
	if err == nil {
		t.Fatal("OnStop should return an error after being called after stop")
	}

	if onStopCalled {
		t.Error("OnStop function called immediately despite the thread group being closed already.")
	}
	afterStopCalled := false
	err = tg.AfterStop(func() error {
		afterStopCalled = true
		return nil
	})
	if err == nil {
		t.Fatal("AfterStop should return an error after being called after stop")
	}
	if afterStopCalled {
		t.Error("AfterStop function called immediately despite the thread group being closed already.")
	}
}

// TestThreadGroupConcurrentAdd tests that Add can be called concurrently with Stop.
func TestThreadGroupConcurrentAdd(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	var tg ThreadGroup
	for i := 0; i < 1000; i++ {
		go func() {
			err := tg.Add()
			if err != nil {
				return
			}
			defer tg.Done()

			select {
			case <-time.After(100 * time.Millisecond):
			case <-tg.StopChan():
			}
		}()
	}
	time.Sleep(25 * time.Millisecond)
	err := tg.Stop()
	if err != nil {
		t.Fatal(err)
	}
}

// TestThreadGroupOnce tests that a zero-valued ThreadGroup's stopChan is
// properly initialized.
func TestThreadGroupOnce(t *testing.T) {
	tg := new(ThreadGroup)
	if tg.stopCtx != nil {
		t.Error("expected nil stopCtx")
	}

	// these methods should cause.stopCtx to be initialized
	tg.StopChan()
	if tg.stopCtx == nil {
		t.Error("stopCtx should have been initialized by StopChan")
	}

	tg = new(ThreadGroup)
	tg.isStopped()
	if tg.stopCtx == nil {
		t.Error("stopCtx should have been initialized by isStopped")
	}

	tg = new(ThreadGroup)
	err := tg.Add()
	if err != nil {
		t.Error(err)
	}
	if tg.stopCtx == nil {
		t.Error("stopCtx should have been initialized by Add")
	}

	tg = new(ThreadGroup)
	err = tg.Stop()
	if err != nil {
		t.Error(err)
	}
	if tg.stopCtx == nil {
		t.Error("stopCtx should have been initialized by Stop")
	}
}

// TestThreadGroupOnStop tests that Stop calls functions registered with
// OnStop.
func TestThreadGroupOnStop(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	l, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}

	// create ThreadGroup and register the closer
	var tg ThreadGroup
	err = tg.OnStop(func() error { return l.Close() })
	if err != nil {
		t.Fatal(err)
	}

	// send on channel when listener is closed
	var closed bool
	err = tg.Add()
	if err != nil {
		t.Error(err)
	}
	go func() {
		defer tg.Done()
		_, err := l.Accept()
		closed = err != nil
	}()

	err = tg.Stop()
	if err != nil {
		t.Error(err)
	}
	if !closed {
		t.Fatal("Stop did not close listener")
	}
}

// TestThreadGroupRace tests that calling ThreadGroup methods concurrently
// does not trigger the race detector.
func TestThreadGroupRace(t *testing.T) {
	var tg ThreadGroup
	go tg.StopChan()
	go func() {
		if tg.Add() == nil {
			tg.Done()
		}
	}()
	err := tg.Stop()
	if err != nil {
		t.Fatal(err)
	}
}

// TestThreadGroupCloseAfterStop checks that an AfterStop function is
// correctly called after the thread is stopped.
func TestThreadGroupClosedAfterStop(t *testing.T) {
	var tg ThreadGroup
	var closed bool
	err := tg.AfterStop(func() error { closed = true; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if closed {
		t.Fatal("close function should not have been called yet")
	}
	if err := tg.Stop(); err != nil {
		t.Fatal(err)
	}
	if !closed {
		t.Fatal("close function should have been called")
	}

	// Stop has already been called, so the close function should not be called.
	closed = false
	err = tg.AfterStop(func() error { closed = true; return nil })
	if err == nil {
		t.Fatal("AfterStop should return an error after stop")
	}
	if closed {
		t.Fatal("close function should not have been called")
	}
}

// TestThreadGroupNetworkExample tries to use a thread group as it might be
// expected to be used by a networking module.
func TestThreadGroupNetworkExample(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	var tg ThreadGroup

	// Open a listener, and queue the shutdown.
	listenerCleanedUp := false
	listener, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	// Open a thread to accept calls from the listener.
	handlerFinishedChan := make(chan struct{})
	go func() {
		// Threadgroup shutdown should stall until listener closes.
		err := tg.Add()
		if err != nil {
			// Testing is non-deterministic, sometimes Stop() will be called
			// before the listener fully starts up.
			close(handlerFinishedChan)
			return
		}
		defer tg.Done()

		for {
			_, err := listener.Accept()
			if err != nil {
				break
			}
		}
		close(handlerFinishedChan)
	}()
	err = tg.OnStop(func() error {
		err := listener.Close()
		if err != nil {
			return err
		}
		<-handlerFinishedChan

		listenerCleanedUp = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Create a thread that does some stuff which takes time, and then closes.
	threadFinished := false
	err = tg.Add()
	if err != nil {
		t.Fatal(err)
	}
	go func() error {
		time.Sleep(time.Second)
		threadFinished = true
		tg.Done()
		return nil
	}()

	// Create a thread that does some stuff which takes time, and then closes.
	// Use Stop to wait for the threead to finish and then check that all
	// resources have closed.
	threadFinished2 := false
	err = tg.Add()
	if err != nil {
		t.Fatal(err)
	}
	go func() error {
		time.Sleep(time.Second)
		threadFinished2 = true
		tg.Done()
		return nil
	}()

	// Let the listener run for a bit.
	time.Sleep(100 * time.Millisecond)
	err = tg.Stop()
	if err != nil {
		t.Fatal(err)
	}
	if !threadFinished || !threadFinished2 || !listenerCleanedUp {
		t.Error("stop did not block until all running resources had closed")
	}
}

// TestNestedAdd will call Add repeatedly from the same goroutine, then call
// stop concurrently.
func TestNestedAdd(t *testing.T) {
	var tg ThreadGroup
	go func() {
		for i := 0; i < 1000; i++ {
			err := tg.Add()
			if err == nil {
				defer tg.Done()
			}
		}
	}()

	time.Sleep(10 * time.Millisecond)
	err := tg.Stop()
	if err != nil {
		t.Error(err)
	}
}

// TestAddOnStop checks that you can safely call OnStop from under the
// protection of an Add call.
func TestAddOnStop(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}

	var tg ThreadGroup
	var data int
	addChan := make(chan struct{})
	stopChan := make(chan struct{})
	err := tg.OnStop(func() error {
		close(stopChan)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		err := tg.Add()
		if err != nil {
			t.Fatal(err)
		}
		close(addChan)

		// Wait for the call to 'Stop' to be called in the parent thread, and
		// then queue a bunch of 'OnStop' and 'AfterStop' functions before
		// calling 'Done'.
		<-stopChan
		for i := 0; i < 10; i++ {
			err = tg.OnStop(func() error {
				data++
				return nil
			})
			if err == nil {
				t.Fatal("OnStop should return an error when being called after stop")
			}
			err = tg.AfterStop(func() error {
				data++
				return nil
			})
			if err == nil {
				t.Fatal("AfterStop should return an error when being called after stop")
			}
		}
		tg.Done()
	}()

	// Wait for 'Add' to be called in the above thread, to guarantee that
	// OnStop and AfterStop will be called after 'Add' and 'Stop' have been
	// called together.
	<-addChan
	err = tg.Stop()
	if err != nil {
		t.Fatal(err)
	}

	if data != 0 {
		t.Error("data should not be incremented after thread group is stopped", data)
	}
}

// TestLaunch checks that launching a goroutine from a thread group works
// correctly.
func TestLaunch(t *testing.T) {
	var tg ThreadGroup

	// Create a function which indicates that it finished running. This function
	// will sleep before setting the indicator, to give the thread group time to
	// stop first.
	threadFinished := false
	fn := func() {
		time.Sleep(time.Millisecond * 100)
		threadFinished = true
	}

	// Launch the function, this should wrap it in tg.Add and tg.Done calls,
	// preventing the thraedgroup from stopping until the thread has completed
	// fully.
	err := tg.Launch(fn)
	if err != nil {
		t.Fatal(err)
	}

	// Stop the threadgroup and check that threadFinished has been updated
	// correctly.
	err = tg.Stop()
	if err != nil {
		t.Fatal(err)
	}
	if !threadFinished {
		t.Fatal("thread should have had time to finish")
	}

	// Try to launch the thread again, this should fail because the tg is
	// stopped.
	err = tg.Launch(fn)
	if err == nil {
		t.Fatal("a launch should return an error after the thread group has stopped")
	}
}

// BenchmarkThreadGroup times how long it takes to add a ton of threads and
// trigger goroutines that call Done.
func BenchmarkThreadGroup(b *testing.B) {
	var tg ThreadGroup
	for i := 0; i < b.N; i++ {
		err := tg.Add()
		if err != nil {
			b.Error(err)
		}
		go tg.Done()
	}
	err := tg.Stop()
	if err != nil {
		b.Error(err)
	}
}

// BenchmarkWaitGroup times how long it takes to add a ton of threads to a wait
// group and trigger goroutines that call Done.
func BenchmarkWaitGroup(b *testing.B) {
	var wg sync.WaitGroup
	for i := 0; i < b.N; i++ {
		wg.Add(1)
		go wg.Done()
	}
	wg.Wait()
}
