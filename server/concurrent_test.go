// concurrent_test.go
// Stress test for ConcurrentHMap under many goroutines hitting it at once.
//
// The point of this test is NOT to check a specific return value -- it's to run
// it with the race detector:
//
//	go test -race ./server/
//
// The -race flag reports if two goroutines ever touch the same memory without a
// lock. If our mutex is correct, this passes clean; if it isn't, -race prints the
// exact conflicting reads/writes. For a project whose whole selling point is a
// concurrent map, a clean -race run is real proof the locking works.

package main

import (
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestConcurrentHMap_RaceStress(t *testing.T) {
	store := &ConcurrentHMap{}

	const (
		numGoroutines = 8
		opsPerG       = 2000
		keySpace      = 200 // small key space so goroutines actually collide on keys
	)

	var wg sync.WaitGroup

	for g := 0; g < numGoroutines; g++ {
		wg.Add(1)

		go func(g int) {
			defer wg.Done()

			for i := 0; i < opsPerG; i++ {
				// a bounded key space means many goroutines fight over the same
				// keys -- exactly the contention we want to stress.
				key := "key:" + strconv.Itoa(i%keySpace)

				// mix all the operations so set / get / del / ttl run concurrently,
				// including while progressive rehashing may be happening.
				switch i % 4 {
				case 0:
					store.Set(key, "value")
				case 1:
					store.Get(key)
				case 2:
					store.Del(key)
				case 3:
					store.SetTTL(key, "value", 60)
				}
			}
		}(g)
	}

	wg.Wait()

	// also give the sweeper a spin concurrently once, just to exercise that path
	// under the same lock. (no assertion -- we only care that -race stays quiet.)
	store.SweepExpired(time.Now().UnixNano(), 16)
}
