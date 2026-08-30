package main

import (
	"flag"
	"log"
	"net/http"
	"time"

	"github.com/NotYaniV/RailGun/bench/internal/origin"
)

func main() {
	addr := flag.String("addr", ":9001", "listen address")
	delay := flag.Duration("delay", 0, "artificial upstream delay per request")
	flag.Parse()

	srv := &http.Server{
		Addr:              *addr,
		Handler:           origin.Handler(origin.Config{Delay: *delay}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("origin listening on %s (delay=%v)", *addr, *delay)
	log.Fatal(srv.ListenAndServe())
}
