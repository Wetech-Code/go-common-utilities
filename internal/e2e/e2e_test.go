// Package e2e checks that the service building blocks work together: one service calls
// another through a gateway, with a single request ID end to end, a header forwarded only
// when asked, input validated on arrival, and everything shut down cleanly.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wetech-Code/go-common-utilities/httpclient"
	"github.com/Wetech-Code/go-common-utilities/logger"
	"github.com/Wetech-Code/go-common-utilities/middleware"
	"github.com/Wetech-Code/go-common-utilities/reqctx"
	"github.com/Wetech-Code/go-common-utilities/server"
	"github.com/Wetech-Code/go-common-utilities/validator"
)

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

func newLogger(buf *lockedBuf) *slog.Logger {
	l, _ := logger.New(logger.Config{Output: buf, ContextAttrs: logger.JoinContextAttrs(reqctx.Attrs)})
	return l
}

func run(t *testing.T, srv *server.Server) (stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	select {
	case <-srv.Ready():
	case err := <-done:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("not ready")
	}
	var once sync.Once
	var result error
	stop = func() error {
		once.Do(func() { cancel(); result = <-done })
		return result
	}
	t.Cleanup(func() { stop() })
	return stop
}

type captureRequest struct {
	ChargeID string `path:"chargeId" validate:"required,identifier"`
	Amount   int    `body:"amount" validate:"required,min=1,max=1000000"`
	TenantID string `header:"X-Tenant-ID" validate:"required,identifier"`
	Key      string `header:"X-Idempotency-Key" validate:"required,max=64"`
}

func TestTwoServicesThroughAGatewayShareOneRequestID(t *testing.T) {
	paymentsLog, ordersLog := &lockedBuf{}, &lockedBuf{}

	// The payments service: edge middleware, validator, handler.
	var mu sync.Mutex
	applied := map[string]int{} // idempotency key -> times applied: the service keeps the receipt
	mux := http.NewServeMux()
	mux.HandleFunc("POST /charges/{chargeId}/capture", func(w http.ResponseWriter, r *http.Request) {
		var req captureRequest
		if err := validator.Bind(r, &req); err != nil {
			var verr *validator.Errors
			if errors.As(err, &verr) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(verr.StatusCode())
				w.Write(verr.JSON())
				return
			}
			http.Error(w, "internal", 500)
			return
		}
		mu.Lock()
		first := applied[req.Key] == 0
		applied[req.Key]++
		mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"captured": req.Amount, "first": first, "tenant": reqctx.HeaderValue(r.Context(), "X-Tenant-ID"), "request_id": reqctx.RequestID(r.Context())})
	})
	paymentsHandler := middleware.Chain(mux,
		middleware.RequestID(), middleware.AccessLog(newLogger(paymentsLog)), middleware.Recover(newLogger(paymentsLog)),
		middleware.CaptureHeaders("X-Tenant-ID"), middleware.MaxBody(1<<16))
	paymentsSrv, _ := server.New(server.Config{Addr: "127.0.0.1:0", Handler: paymentsHandler, Logger: newLogger(paymentsLog)})
	run(t, paymentsSrv)

	// The gateway: routes /api/payments/... to the payments service and strips the prefix.
	target, _ := url.Parse("http://" + paymentsSrv.Addr().String())
	rp := httputil.NewSingleHostReverseProxy(target)
	director := rp.Director
	rp.Director = func(r *http.Request) {
		r.URL.Path = strings.TrimPrefix(r.URL.Path, "/api/payments")
		r.URL.RawPath = ""
		director(r)
	}
	gatewaySrv, _ := server.New(server.Config{Addr: "127.0.0.1:0", Handler: rp, Logger: newLogger(paymentsLog)})
	run(t, gatewaySrv)

	// The orders service calls payments through the gateway.
	api, err := httpclient.New(httpclient.Config{BaseURL: "http://" + gatewaySrv.Addr().String(), PathPrefix: "/api", AllowedPrefixes: []string{"payments"},
		Headers: map[string]string{"X-Caller": "orders"}})
	if err != nil {
		t.Fatal(err)
	}
	payments, err := api.WithPrefix("payments")
	if err != nil {
		t.Fatal(err)
	}
	ordersMux := http.NewServeMux()
	ordersMux.HandleFunc("POST /orders/{orderId}/pay", func(w http.ResponseWriter, r *http.Request) {
		var out map[string]any
		err := payments.JSON(r.Context(), http.MethodPost, httpclient.Path("charges", "ch-"+r.PathValue("orderId"), "capture"),
			map[string]int{"amount": 300}, &out,
			httpclient.ForwardFromContext("X-Tenant-ID"), httpclient.IdempotencyKey("order-"+r.PathValue("orderId")+"-capture"))
		if err != nil {
			var se *httpclient.StatusError
			if errors.As(err, &se) {
				w.WriteHeader(se.Status)
				w.Write(se.Body)
				return
			}
			http.Error(w, "upstream failure", http.StatusBadGateway)
			return
		}
		json.NewEncoder(w).Encode(out)
	})
	ordersHandler := middleware.Chain(ordersMux, middleware.RequestID(middleware.AlwaysGenerate()), middleware.AccessLog(newLogger(ordersLog)),
		middleware.Recover(newLogger(ordersLog)), middleware.CaptureHeaders("X-Tenant-ID"))
	ordersSrv, _ := server.New(server.Config{Addr: "127.0.0.1:0", Handler: ordersHandler, Logger: newLogger(ordersLog)})
	closed := []string{}
	ordersSrv.OnShutdown("outbound client", func(context.Context) error { closed = append(closed, "outbound client"); return nil })
	stopOrders := run(t, ordersSrv)

	post := func(path string, hdr map[string][]string) (*http.Response, []byte) {
		req, _ := http.NewRequest("POST", "http://"+ordersSrv.Addr().String()+path, strings.NewReader(`{}`))
		for k, v := range hdr {
			req.Header[k] = v
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, b
	}

	resp, body := post("/orders/O42/pay", map[string][]string{"X-Tenant-Id": {"tenant-3"}, "X-Request-Id": {"client-chosen-id"}})
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var got map[string]any
	json.Unmarshal(body, &got)
	rid := resp.Header.Get("X-Request-ID")
	if rid == "" || rid == "client-chosen-id" || got["request_id"] != rid {
		t.Errorf("one request ID end to end: response header %q, payments saw %v", rid, got["request_id"])
	}
	if got["captured"] != float64(300) || got["tenant"] != "tenant-3" || got["first"] != true {
		t.Errorf("payments result = %v", got)
	}
	for name, buf := range map[string]*lockedBuf{"orders": ordersLog, "payments": paymentsLog} {
		if !strings.Contains(buf.String(), `"request_id":"`+rid+`"`) {
			t.Errorf("%s log lacks request_id %s:\n%s", name, rid, buf.String())
		}
	}

	// The same call again carries the same idempotency key; the library supplies the key and the
	// service, which keeps the receipt, applies it once.
	_, b2 := post("/orders/O42/pay", map[string][]string{"X-Tenant-Id": {"tenant-3"}})
	var again map[string]any
	json.Unmarshal(b2, &again)
	if again["first"] != false {
		t.Errorf("a repeat with the same idempotency key must not be applied twice: %v", again)
	}

	// A header that was not forwarded is not there: the payments validator refuses the call, and
	// its 400 body passes through untouched.
	resp, b3 := post("/orders/O43/pay", nil)
	if resp.StatusCode != 400 || !strings.Contains(string(b3), `"field":"X-Tenant-ID"`) {
		t.Errorf("missing header: %d %s", resp.StatusCode, b3)
	}
	// Duplicate values of a captured header are rejected at the first service already.
	resp, _ = post("/orders/O44/pay", map[string][]string{"X-Tenant-Id": {"one", "two"}})
	if resp.StatusCode != 400 {
		t.Errorf("duplicate captured header: %d", resp.StatusCode)
	}

	if err := stopOrders(); err != nil || strings.Join(closed, ",") != "outbound client" {
		t.Errorf("clean shutdown: err=%v closed=%v", err, closed)
	}
}
