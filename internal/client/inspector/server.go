package inspector

import (
	_ "embed"
	"net/http"
)

//go:embed index.html
var indexHTML []byte

func Start(port string) {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write(indexHTML)
	})

	// API to get requests (TODO)
	http.HandleFunc("/api/requests", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("[]"))
	})

	go http.ListenAndServe(":"+port, nil)
}
