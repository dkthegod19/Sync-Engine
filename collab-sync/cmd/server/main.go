// Command server runs the collaborative editor: an HTTP server that serves the
// browser client from web/ and exposes the realtime sync hub at /ws.
//
//	go run ./cmd/server            # listens on :8080, serves ./web
//	go run ./cmd/server -addr :9000 -web ./web
package main

import (
	"flag"
	"log"
	"net/http"

	"collabsync/internal/server"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	webDir := flag.String("web", "./web", "directory of static client files")
	flag.Parse()

	hub := server.NewHub()

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", hub.ServeWS)
	mux.Handle("/", noCache(http.FileServer(http.Dir(*webDir))))

	log.Printf("collab-sync listening on http://localhost%s  (open it in two browser windows)", *addr)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatal(err)
	}
}

// noCache keeps the demo simple to iterate on.
func noCache(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		h.ServeHTTP(w, r)
	})
}
