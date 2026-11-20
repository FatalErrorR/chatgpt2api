// Command fakeupstream runs internal/testsupport/fakeupstream as a server for the contract,
// end-to-end and load tests.
//
// Usage: go run ./tools/fakeupstream -addr 127.0.0.1:18100 -delay 2s
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/basketikun/chatgpt2api/internal/testsupport/fakeupstream"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:18100", "listen address")
	delay := flag.Duration("delay", 2*time.Second, "time until an image is ready")
	slowDelay := flag.Duration("slow-delay", 120*time.Second, "time until an image is ready for the slow scenario")
	logRequests := flag.Bool("log", false, "log each request: arrival (unix ms), method, path, time to finish (ms)")
	uniqueImages := flag.Bool("unique-images", false, "serve a different image for every file (load tests)")
	flag.Parse()
	server := fakeupstream.New("http://"+*addr, *delay, *slowDelay)
	server.UniqueImages = *uniqueImages
	log.Printf("fake upstream on http://%s (delay %s)", *addr, *delay)
	var handler http.Handler = server
	if *logRequests {
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			started := time.Now()
			server.ServeHTTP(w, r)
			fmt.Printf("%d %s %s %d\n", started.UnixMilli(), r.Method, r.URL.Path, time.Since(started).Milliseconds())
		})
	}
	log.Fatal(http.ListenAndServe(*addr, handler))
}
