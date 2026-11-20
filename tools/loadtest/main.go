// Command loadtest drives acceptance F1–F3 against a running server backed by the fake upstream:
// N workers send image generations (one each, or back to back for -duration), another loop times
// the admin endpoints meanwhile, and the server's resident memory is sampled every 10 seconds.
// Prints a summary and exits 1 when a request failed.
//
//	go run ./tools/loadtest -base http://127.0.0.1:3500 -key <admin key> -pid <server pid> -concurrency 300
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type sample struct {
	at  time.Duration
	rss int64 // KiB
}

func percentile(values []time.Duration, p float64) time.Duration {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[min(len(sorted)-1, int(float64(len(sorted))*p))]
}

func rssKiB(pid int) int64 {
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return -1
	}
	value, _ := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	return value
}

// slope is the least-squares growth of the samples in MiB per hour.
func slope(samples []sample) float64 {
	if len(samples) < 3 {
		return 0
	}
	var n, sx, sy, sxx, sxy float64
	for _, s := range samples {
		x, y := s.at.Hours(), float64(s.rss)/1024
		n, sx, sy, sxx, sxy = n+1, sx+x, sy+y, sxx+x*x, sxy+x*y
	}
	if d := n*sxx - sx*sx; d != 0 {
		return (n*sxy - sx*sy) / d
	}
	return 0
}

func main() {
	base := flag.String("base", "http://127.0.0.1:3500", "server")
	key := flag.String("key", "", "admin key")
	pid := flag.Int("pid", 0, "server pid, for resident memory")
	concurrency := flag.Int("concurrency", 300, "concurrent image requests")
	duration := flag.Duration("duration", 0, "keep every worker sending for this long; 0 sends one request each")
	adminEvery := flag.Duration("admin-every", 500*time.Millisecond, "pause between admin calls")
	flag.Parse()

	client := &http.Client{Timeout: 10 * time.Minute, Transport: &http.Transport{Proxy: nil, MaxIdleConns: 2000, MaxIdleConnsPerHost: 2000, MaxConnsPerHost: 0}}
	do := func(method, path string, body any) (int, []byte, time.Duration, error) {
		var reader io.Reader
		if body != nil {
			raw, _ := json.Marshal(body)
			reader = bytes.NewReader(raw)
		}
		request, _ := http.NewRequest(method, *base+path, reader)
		request.Header.Set("Authorization", "Bearer "+*key)
		request.Header.Set("Content-Type", "application/json")
		started := time.Now()
		response, err := client.Do(request)
		if err != nil {
			return 0, nil, time.Since(started), err
		}
		defer response.Body.Close()
		raw, _ := io.ReadAll(response.Body)
		return response.StatusCode, raw, time.Since(started), nil
	}

	start := time.Now()
	var mu sync.Mutex
	var imageLatency []time.Duration
	statuses := map[string]int{}
	var failures []string
	adminLatency := map[string][]time.Duration{}
	var samples []sample
	var sent atomic.Int64
	stop := make(chan struct{})

	// resident memory
	var background sync.WaitGroup
	if *pid > 0 {
		background.Add(1)
		go func() {
			defer background.Done()
			for {
				mu.Lock()
				samples = append(samples, sample{time.Since(start), rssKiB(*pid)})
				mu.Unlock()
				select {
				case <-stop:
					return
				case <-time.After(10 * time.Second):
				}
			}
		}()
	}
	// admin endpoints while the images run
	background.Add(1)
	go func() {
		defer background.Done()
		paths := []string{"/api/accounts", "/api/logs", "/api/settings", "/api/proxies", "/api/logs/image-counts", "/health?format=json"}
		for i := 0; ; i++ {
			path := paths[i%len(paths)]
			status, _, latency, err := do("GET", path, nil)
			mu.Lock()
			adminLatency[path] = append(adminLatency[path], latency)
			if err != nil || status != 200 {
				failures = append(failures, fmt.Sprintf("GET %s %d %v", path, status, err))
			}
			mu.Unlock()
			select {
			case <-stop:
				return
			case <-time.After(*adminEvery):
			}
		}
	}()

	var workers sync.WaitGroup
	deadline := start.Add(*duration)
	for w := 0; w < *concurrency; w++ {
		workers.Add(1)
		go func(w int) {
			defer workers.Done()
			for {
				n := sent.Add(1)
				status, raw, latency, err := do("POST", "/v1/images/generations", map[string]any{"prompt": fmt.Sprintf("load test %d", n), "response_format": "url"})
				mu.Lock()
				imageLatency = append(imageLatency, latency)
				statuses[strconv.Itoa(status)]++
				if err != nil || status != 200 || !bytes.Contains(raw, []byte(`"url"`)) {
					failures = append(failures, fmt.Sprintf("image %d: %d %v %.200s", n, status, err, raw))
				}
				mu.Unlock()
				if *duration == 0 || time.Now().After(deadline) {
					return
				}
			}
		}(w)
	}
	workers.Wait()
	close(stop)
	background.Wait()
	if *pid > 0 {
		samples = append(samples, sample{time.Since(start), rssKiB(*pid)})
	}

	fmt.Printf("用时 %s，生图请求 %d 个，状态 %v\n", time.Since(start).Round(time.Second), len(imageLatency), statuses)
	fmt.Printf("生图耗时 p50 %s  p95 %s  p99 %s  最长 %s\n", percentile(imageLatency, .5).Round(time.Millisecond),
		percentile(imageLatency, .95).Round(time.Millisecond), percentile(imageLatency, .99).Round(time.Millisecond), percentile(imageLatency, 1).Round(time.Millisecond))
	var all []time.Duration
	names := make([]string, 0, len(adminLatency))
	for path := range adminLatency {
		names = append(names, path)
	}
	sort.Strings(names)
	for _, path := range names {
		values := adminLatency[path]
		all = append(all, values...)
		fmt.Printf("管理接口 %-26s %4d 次  p50 %-8s p95 %s\n", path, len(values), percentile(values, .5).Round(time.Millisecond), percentile(values, .95).Round(time.Millisecond))
	}
	fmt.Printf("管理接口合计 p95 %s\n", percentile(all, .95).Round(time.Millisecond))
	if len(samples) > 0 {
		lo, hi := samples[0].rss, samples[0].rss
		for _, s := range samples {
			lo, hi = min(lo, s.rss), max(hi, s.rss)
		}
		half := samples[len(samples)/2:]
		fmt.Printf("常驻内存 开始 %d MiB  最低 %d  最高 %d  结束 %d MiB；后半段斜率 %.1f MiB/小时（%d 个采样）\n",
			samples[0].rss/1024, lo/1024, hi/1024, samples[len(samples)-1].rss/1024, slope(half), len(samples))
		if path := os.Getenv("LOADTEST_RSS_CSV"); path != "" {
			var lines []string
			for _, s := range samples {
				lines = append(lines, fmt.Sprintf("%.0f,%d", s.at.Seconds(), s.rss))
			}
			os.WriteFile(path, []byte("seconds,rss_kib\n"+strings.Join(lines, "\n")+"\n"), 0o644)
		}
	}
	if len(failures) > 0 {
		fmt.Printf("失败 %d 个，前 5 个：\n  %s\n", len(failures), strings.Join(failures[:min(5, len(failures))], "\n  "))
		os.Exit(1)
	}
}
