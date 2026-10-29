package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

func main() {
	url := flag.String("url", "http://localhost:8080/api/v1/checkout", "checkout endpoint")
	n := flag.Int("requests", 100000, "requests to send")
	d := flag.Duration("duration", 10*time.Second, "load window")
	concurrency := flag.Int("concurrency", 512, "concurrent clients")
	flag.Parse()
	client := &http.Client{Timeout: 15 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), *d+20*time.Second)
	defer cancel()
	start := time.Now()
	var sent, ok, throttled, failed int64
	jobs := make(chan int)
	var wg sync.WaitGroup
	for i := 0; i < *concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range jobs {
				req, _ := http.NewRequestWithContext(ctx, http.MethodPost, *url, nil)
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("X-User-ID", fmt.Sprintf("load-user-%d", time.Now().UnixNano()))
				req.Header.Set("Idempotency-Key", fmt.Sprintf("load-%d", time.Now().UnixNano()))
				req.Body = http.NoBody
				atomic.AddInt64(&sent, 1)
				resp, err := client.Do(req)
				if err != nil {
					atomic.AddInt64(&failed, 1)
					continue
				}
				if resp.StatusCode == http.StatusTooManyRequests {
					atomic.AddInt64(&throttled, 1)
				} else if resp.StatusCode < 500 {
					atomic.AddInt64(&ok, 1)
				} else {
					atomic.AddInt64(&failed, 1)
				}
				resp.Body.Close()
			}
		}()
	}
	for i := 0; i < *n; i++ {
		select {
		case jobs <- i:
		case <-ctx.Done():
			break
		}
	}
	close(jobs)
	wg.Wait()
	elapsed := time.Since(start)
	fmt.Printf("requests=%d ok_or_client_handled=%d throttled=%d failed=%d elapsed=%s achieved_rps=%.2f\n", sent, ok, throttled, failed, elapsed.Round(time.Millisecond), float64(sent)/elapsed.Seconds())
}

