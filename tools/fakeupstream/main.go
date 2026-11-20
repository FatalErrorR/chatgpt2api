// Command fakeupstream runs internal/testsupport/fakeupstream as a server for the contract,
// end-to-end and load tests.
//
// Usage: go run ./tools/fakeupstream -addr 127.0.0.1:18100 -delay 2s
package main

import (
	"flag"
	"log"
	"net/http"
	"time"

	"github.com/basketikun/chatgpt2api/internal/testsupport/fakeupstream"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:18100", "listen address")
	delay := flag.Duration("delay", 2*time.Second, "time until an image is ready")
	slowDelay := flag.Duration("slow-delay", 120*time.Second, "time until an image is ready for the slow scenario")
	flag.Parse()
	server := fakeupstream.New("http://"+*addr, *delay, *slowDelay)
	log.Printf("fake upstream on http://%s (delay %s)", *addr, *delay)
	log.Fatal(http.ListenAndServe(*addr, server))
}
