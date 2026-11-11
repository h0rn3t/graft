// Package parallel runs independent, CPU-bound steps on every available CPU.
package parallel

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
)

// Do calls fn(i) once for each i in [0, n) and returns when every call has.
// Calls run on up to GOMAXPROCS goroutines, so fn must be safe to call
// concurrently and each call should write only what belongs to its own i.
//
// A panic in fn stops the remaining calls and is raised again on the calling
// goroutine, as a string holding the value and the stack that raised it, so a
// caller that recovers its own panics also contains the ones of its workers.
func Do(n int, fn func(i int)) {
	workers := min(runtime.GOMAXPROCS(0), n)
	if workers <= 1 {
		for i := range n {
			fn(i)
		}
		return
	}
	var next atomic.Int64
	var failure atomic.Pointer[string]
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			defer func() {
				if value := recover(); value != nil {
					next.Store(int64(n)) // the other workers stop at their next index
					failure.CompareAndSwap(nil, new(fmt.Sprintf("%v\n\n%s", value, debug.Stack())))
				}
			}()
			for i := next.Add(1) - 1; i < int64(n); i = next.Add(1) - 1 {
				fn(int(i))
			}
		})
	}
	wg.Wait()
	if message := failure.Load(); message != nil {
		panic(*message)
	}
}
