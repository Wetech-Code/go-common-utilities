package middleware_test

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"

	"github.com/Wetech-Code/go-common-utilities/middleware"
	"github.com/Wetech-Code/go-common-utilities/reqctx"
)

func Example() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ping", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "pong for %s", reqctx.RequestID(r.Context()))
	})
	h := middleware.Chain(mux,
		middleware.RequestID(), // first: every later step and log line has the ID
		middleware.AccessLog(log),
		middleware.Recover(log),
	)
	r := httptest.NewRequest("GET", "/ping", nil)
	r.Header.Set("X-Request-ID", "req-1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	fmt.Println(w.Body.String(), w.Header().Get("X-Request-ID"))
	// Output: pong for req-1 req-1
}
