package inspector

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

//go:embed index.html
var indexHTML []byte

type RequestInfo struct {
	ID        int64     `json:"id"`
	Method    string    `json:"method"`
	Host      string    `json:"host"`
	Path      string    `json:"path"`
	Status    int       `json:"status"`
	Timestamp time.Time `json:"timestamp"`
}

var (
	requests []RequestInfo
	mu       sync.Mutex
)

func AddRequest(method, host, path string, status int) {
	mu.Lock()
	defer mu.Unlock()
	req := RequestInfo{
		ID:        time.Now().UnixNano(),
		Method:    method,
		Host:      host,
		Path:      path,
		Status:    status,
		Timestamp: time.Now(),
	}
	requests = append([]RequestInfo{req}, requests...)
	if len(requests) > 100 {
		requests = requests[:100]
	}
}

func Start(port string) {
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write(indexHTML)
	})

	mux.HandleFunc("/api/requests", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(requests)
	})

	go http.ListenAndServe(":"+port, mux)
}
