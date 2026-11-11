package parallel

import (
	"strings"
	"sync/atomic"
	"testing"
)

func TestDoRunsEveryIndexOnce(t *testing.T) {
	for _, n := range []int{0, 1, 2, 1000} {
		calls := make([]atomic.Int32, n)
		Do(n, func(i int) { calls[i].Add(1) })
		for i := range calls {
			if got := calls[i].Load(); got != 1 {
				t.Errorf("Do(%d, fn) called fn(%d) %d times, want 1", n, i, got)
			}
		}
	}
}

func TestDoRaisesAWorkerPanicOnTheCaller(t *testing.T) {
	for _, n := range []int{1, 64} {
		var recovered any
		func() {
			defer func() { recovered = recover() }()
			Do(n, func(i int) {
				if i == n-1 {
					panic("boom at the last index")
				}
			})
		}()
		message, _ := recovered.(string)
		if !strings.Contains(message, "boom at the last index") {
			t.Errorf("Do(%d, fn) with a panicking fn recovered %v, want the panic value", n, recovered)
		}
	}
}
