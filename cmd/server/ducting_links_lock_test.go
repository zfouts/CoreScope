package main

import (
	"sync"
	"testing"
	"time"
)

// BenchmarkDuctingLinkReceptionsWriterWait: how long an ingest-style writer
// (Lock/Unlock in a loop) waits for the store lock while the scan runs on a
// 50k-advert store; max-wait-us is the longest single wait.
func BenchmarkDuctingLinkReceptionsWriterWait(b *testing.B) {
	ends, txs := linksManyAdverts(50000)
	srv := linksServer(&Config{}, nil, txs...)
	from, to := linksNow.Unix()-30*86400, linksNow.Unix()
	var maxWait time.Duration
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			t0 := time.Now()
			srv.store.mu.Lock()
			w := time.Since(t0)
			srv.store.mu.Unlock()
			maxWait = max(maxWait, w)
			time.Sleep(50 * time.Microsecond)
		}
	}()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		srv.store.ductingLinkReceptions(ends, srv.cfg, from, to)
	}
	b.StopTimer()
	close(stop)
	wg.Wait()
	b.ReportMetric(float64(maxWait.Microseconds()), "max-wait-us")
}
