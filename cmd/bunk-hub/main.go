// bunk-hub is the blind E2E-encrypted relay server for bunk.
//
// Run it on any always-on, reachable box (e.g. a free Oracle Cloud
// instance). Clients dial out to it; it never sees plaintext traffic.
package main

import (
	"flag"
	"log"
	"net/http"
	"os"

	"bunk/internal/hub"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address (http/ws)")
	dbPath := flag.String("db", "bunk-hub.db", "sqlite database path")
	token := flag.String("token", os.Getenv("BUNK_HUB_TOKEN"), "shared secret clients must present")
	flag.Parse()

	store, err := hub.Open(*dbPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer store.Close()

	h := hub.New(store, *token)

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", h.ServeHTTP)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		m, _ := h.Health()
		w.Write([]byte(`{"status":"ok","machines":` + itoa(m["machines"]) + `}`))
	})

	if *token == "" {
		log.Printf("WARNING: BUNK_HUB_TOKEN is empty — any client can register. Set it in production.")
	}
	log.Printf("bunk-hub listening on %s (db=%s)", *addr, *dbPath)
	log.Printf("point clients at ws://<this-host>%s/ws", *addr)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatal(err)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
