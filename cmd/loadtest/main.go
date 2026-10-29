package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

func main() {
	url := flag.String("url", "http://localhost:8080/api/v1/checkout", "checkout endpoint")
	n := flag.Int("requests", 100000, "requests to launch")
	window := flag.Duration("launch-window", 10*time.Second, "strict request launch window")
	concurrency := flag.Int("concurrency", 400, "maximum HTTP connections per host")
	maxConns := flag.Int("max-connections-per-host", 0, "transport connection limit; defaults to concurrency")
	requestTimeout := flag.Duration("request-timeout", 15*time.Second, "per-request response timeout")
	generatorID := flag.String("generator-id", os.Getenv("LOADGEN_ID"), "unique generator identifier")
	startAt := flag.Int64("start-at", 0, "Unix epoch milliseconds at which to begin launching")
	flag.Parse()
	if *generatorID == "" {
		*generatorID = strconv.Itoa(os.Getpid())
	}
	if *maxConns <= 0 {
		*maxConns = *concurrency
	}
	if *maxConns < 1 {
		*maxConns = 1
	}
	if *startAt > 0 {
		if d := time.Until(time.UnixMilli(*startAt)); d > 0 {
			time.Sleep(d)
		}
	}

	transport := &http.Transport{MaxConnsPerHost: *maxConns, MaxIdleConns: *maxConns * 2, MaxIdleConnsPerHost: *maxConns, IdleConnTimeout: 90 * time.Second}
	client := &http.Client{Transport: transport, Timeout: *requestTimeout}
	launchStart := time.Now()
	launchDeadline := launchStart.Add(*window)

	var launched, completed, accepted, throttled, controlled, failed, timeouts, connections int64
	var wg sync.WaitGroup
	for i := 0; i < *n; i++ {
		if time.Now().After(launchDeadline) {
			break
		}
		id := i + 1
		atomic.AddInt64(&launched, 1)
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), *requestTimeout)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, *url, strings.NewReader(`{"product_id":"phone-500","quantity":1}`))
			if err != nil {
				atomic.AddInt64(&failed, 1)
				return
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-User-ID", fmt.Sprintf("load-user-%s-%d", *generatorID, id))
			req.Header.Set("Idempotency-Key", fmt.Sprintf("load-%s-%d", *generatorID, id))
			resp, err := client.Do(req)
			if err != nil {
				if errors.Is(err, context.DeadlineExceeded) || (func() bool { n, ok := err.(net.Error); return ok && n.Timeout() })() {
					atomic.AddInt64(&timeouts, 1)
				} else {
					atomic.AddInt64(&connections, 1)
				}
				atomic.AddInt64(&failed, 1)
				return
			}
			defer resp.Body.Close()
			atomic.AddInt64(&completed, 1)
			switch {
			case resp.StatusCode == http.StatusAccepted:
				atomic.AddInt64(&accepted, 1)
			case resp.StatusCode == http.StatusTooManyRequests:
				atomic.AddInt64(&throttled, 1)
			case resp.StatusCode < 500:
				atomic.AddInt64(&controlled, 1)
			default:
				atomic.AddInt64(&failed, 1)
			}
		}(id)
	}
	launchElapsed := time.Since(launchStart)
	wg.Wait()
	totalElapsed := time.Since(launchStart)
	inFlight := launched - completed - timeouts - connections
	status := "PASS"
	if launched != int64(*n) {
		status = "FAIL_LAUNCH_WINDOW"
	}
	fmt.Printf("status=%s generator=%s requested=%d launched=%d completed=%d accepted_202=%d throttled_429=%d controlled_lt500=%d failed=%d timeouts=%d connection_failures=%d in_flight=%d launch_elapsed=%s total_elapsed=%s launch_rps=%.2f completion_rps=%.2f\n", status, *generatorID, *n, launched, completed, accepted, throttled, controlled, failed, timeouts, connections, inFlight, launchElapsed.Round(time.Millisecond), totalElapsed.Round(time.Millisecond), float64(launched)/launchElapsed.Seconds(), float64(completed)/totalElapsed.Seconds())
	if status != "PASS" {
		os.Exit(2)
	}
}
