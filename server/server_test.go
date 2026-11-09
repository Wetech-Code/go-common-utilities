package server_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Wetech-Code/go-common-utilities/server"
)

func quietLog() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

func hello() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("hello")) })
}

// start runs srv and waits until it is listening.
func start(t *testing.T, srv *server.Server) (cancel context.CancelFunc, done chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done = make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	select {
	case <-srv.Ready():
	case err := <-done:
		t.Fatalf("server did not start: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("server did not become ready")
	}
	t.Cleanup(cancel)
	return cancel, done
}

func wait(t *testing.T, done chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
		return nil
	}
}

func TestServesAndShutsDownCleanly(t *testing.T) {
	log, buf := quietLog()
	srv, err := server.New(server.Config{Addr: "127.0.0.1:0", Handler: hello(), Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	if srv.Addr() != nil {
		t.Error("Addr must be nil before listening")
	}
	cancel, done := start(t, srv)
	resp, err := http.Get("http://" + srv.Addr().String() + "/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "hello" {
		t.Errorf("body = %q", b)
	}
	cancel()
	if err := wait(t, done); err != nil {
		t.Errorf("a clean shutdown must return nil, got %v", err)
	}
	if _, err := http.Get("http://" + srv.Addr().String() + "/"); err == nil {
		t.Error("the server should no longer accept connections")
	}
	if !strings.Contains(buf.String(), "server listening") || !strings.Contains(buf.String(), "server shutting down") {
		t.Errorf("lifecycle log = %s", buf.String())
	}
	if err := srv.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "already") {
		t.Errorf("a second Run must fail: %v", err)
	}
}

func TestInFlightRequestsFinishDuringShutdown(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	srv, _ := server.New(server.Config{Addr: "127.0.0.1:0", Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(started)
			<-release
			w.Write([]byte("finished"))
		})})
	cancel, done := start(t, srv)

	result := make(chan string, 1)
	go func() {
		resp, err := http.Get("http://" + srv.Addr().String() + "/")
		if err != nil {
			result <- "error: " + err.Error()
			return
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		result <- string(b)
	}()
	<-started
	cancel() // shutdown begins while the request is running
	select {
	case err := <-done:
		t.Fatalf("Run returned before the in-flight request finished: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	close(release)
	if got := <-result; got != "finished" {
		t.Errorf("in-flight request: %q", got)
	}
	if err := wait(t, done); err != nil {
		t.Errorf("Run = %v", err)
	}
}

func TestShutdownTimeoutForcesCloseAndReportsIt(t *testing.T) {
	started := make(chan struct{})
	srv, _ := server.New(server.Config{Addr: "127.0.0.1:0", ShutdownTimeout: 150 * time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(started)
			<-r.Context().Done() // stuck until the connection is cut
		})})
	cancel, done := start(t, srv)
	go http.Get("http://" + srv.Addr().String() + "/")
	<-started
	cancel()
	err := wait(t, done)
	if err == nil || !strings.Contains(err.Error(), "did not finish within") {
		t.Errorf("Run = %v; want a shutdown-timeout error", err)
	}
}

func TestClosersRunInReverseOrderAfterTheServerStops(t *testing.T) {
	log, buf := quietLog()
	var mu sync.Mutex
	var order []string
	srv, _ := server.New(server.Config{Addr: "127.0.0.1:0", Handler: hello(), Logger: log})
	rec := func(name string, err error, panicIt bool) {
		srv.OnShutdown(name, func(ctx context.Context) error {
			if _, ok := ctx.Deadline(); !ok {
				t.Errorf("%s: a closer's context must have a deadline", name)
			}
			mu.Lock()
			order = append(order, name)
			mu.Unlock()
			if panicIt {
				panic("closer exploded")
			}
			return err
		})
	}
	rec("postgres", nil, false)
	rec("cache", errors.New("cache close failed"), false)
	rec("telemetry", nil, true)
	cancel, done := start(t, srv)
	cancel()
	err := wait(t, done)
	if strings.Join(order, ",") != "telemetry,cache,postgres" {
		t.Errorf("order = %v; want reverse of registration, and every closer even after a failure", order)
	}
	if err == nil || !strings.Contains(err.Error(), "closing cache") || !strings.Contains(err.Error(), "closing telemetry") || strings.Contains(err.Error(), "closing postgres") {
		t.Errorf("error = %v", err)
	}
	if !strings.Contains(buf.String(), "shutdown step done") || !strings.Contains(buf.String(), "shutdown step failed") {
		t.Errorf("log = %s", buf.String())
	}
}

func TestListenFailureStillRunsClosers(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	srv, _ := server.New(server.Config{Addr: ln.Addr().String(), Handler: hello(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	closed := false
	srv.OnShutdown("db", func(context.Context) error { closed = true; return nil })
	err = srv.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "listen") || !closed {
		t.Errorf("err=%v closed=%v", err, closed)
	}
	select {
	case <-srv.Ready():
		t.Error("Ready must not be closed when listening failed")
	default:
	}
}

func TestRunUntilSignal(t *testing.T) {
	srv, _ := server.New(server.Config{Addr: "127.0.0.1:0", Handler: hello(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	done := make(chan error, 1)
	go func() { done <- srv.RunUntilSignal() }()
	select {
	case <-srv.Ready():
	case <-time.After(5 * time.Second):
		t.Fatal("not ready")
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := wait(t, done); err != nil {
		t.Errorf("RunUntilSignal = %v", err)
	}
}

func TestConfigValidationAndDefaults(t *testing.T) {
	for name, cfg := range map[string]server.Config{
		"no addr":      {Handler: hello()},
		"no handler":   {Addr: ":0"},
		"negative":     {Addr: ":0", Handler: hello(), ReadTimeout: -1},
		"negative max": {Addr: ":0", Handler: hello(), MaxHeaderBytes: -1},
		"tls no cert":  {Addr: ":0", Handler: hello(), TLSConfig: &tls.Config{}},
	} {
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s: want an error", name)
		}
		if _, err := server.New(cfg); err == nil {
			t.Errorf("%s: New accepted it", name)
		}
	}
	// Slow-client protection is on by default: a client that never finishes its
	// headers is cut off after ReadHeaderTimeout.
	srv, _ := server.New(server.Config{Addr: "127.0.0.1:0", Handler: hello(), ReadHeaderTimeout: 100 * time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	start(t, srv)
	conn, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n")) // never sends the final blank line
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 256)
	n, _ := conn.Read(buf)
	if !strings.Contains(string(buf[:n]), "408") && n != 0 {
		t.Logf("server answered: %q", buf[:n])
	}
	if _, err := conn.Read(buf); err == nil {
		t.Error("the connection should be closed after ReadHeaderTimeout")
	}
}

// ---- TLS ----

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pool *x509.CertPool
}

func newCA(t *testing.T) *testCA {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test ca"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &testCA{cert, key, pool}
}

func (ca *testCA) issue(t *testing.T, cn string, serial int64, server bool) tls.Certificate {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: cn}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	if server {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	} else {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func TestTLSAndMutualTLS(t *testing.T) {
	ca := newCA(t)
	serverCert := ca.issue(t, "server", 2, true)
	clientCert := ca.issue(t, "client", 3, false)

	srv, err := server.New(server.Config{Addr: "127.0.0.1:0", Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cn := "none"
			if len(r.TLS.PeerCertificates) > 0 {
				cn = r.TLS.PeerCertificates[0].Subject.CommonName
			}
			fmt.Fprintf(w, "tls12plus=%v peer=%s", r.TLS.Version >= tls.VersionTLS12, cn)
		}),
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{serverCert}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: ca.pool}})
	if err != nil {
		t.Fatal(err)
	}
	start(t, srv)
	url := "https://" + srv.Addr().String() + "/"

	client := func(cfg *tls.Config) *http.Client {
		return &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}, Timeout: 5 * time.Second}
	}
	resp, err := client(&tls.Config{RootCAs: ca.pool, Certificates: []tls.Certificate{clientCert}}).Get(url)
	if err != nil {
		t.Fatalf("a client with a valid certificate: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "tls12plus=true peer=client" {
		t.Errorf("body = %q", b)
	}
	// No client certificate: refused at the handshake.
	if _, err := client(&tls.Config{RootCAs: ca.pool}).Get(url); err == nil {
		t.Error("mutual TLS must refuse a client without a certificate")
	}
	// A certificate from another CA is refused too.
	other := newCA(t)
	if _, err := client(&tls.Config{RootCAs: ca.pool, Certificates: []tls.Certificate{other.issue(t, "intruder", 9, false)}}).Get(url); err == nil {
		t.Error("mutual TLS must refuse a certificate from an unknown CA")
	}
	// Plain HTTP to a TLS port does not get a working response.
	if resp, err := http.Get("http://" + srv.Addr().String() + "/"); err == nil && resp.StatusCode == 200 {
		t.Error("plain HTTP must not be served on a TLS listener")
	}
}

// ---- health ----

func TestHealth(t *testing.T) {
	var mu sync.Mutex
	dbUp := true
	h := server.Health(map[string]func(context.Context) error{
		"db": func(context.Context) error {
			mu.Lock()
			defer mu.Unlock()
			if !dbUp {
				return errors.New("dial tcp 10.0.0.5:5432: password=hunter2")
			}
			return nil
		},
		"cache": func(context.Context) error { return nil },
		"boom":  func(context.Context) error { panic("check exploded") },
	})
	get := func() (int, string) {
		rec := newRecorder()
		h.ServeHTTP(rec, mustReq())
		return rec.code, rec.body.String()
	}
	code, body := get()
	if code != 503 || !strings.Contains(body, `"failing":["boom"]`) {
		t.Errorf("a panicking check counts as failing: %d %s", code, body)
	}
	mu.Lock()
	dbUp = false
	mu.Unlock()
	code, body = get()
	if code != 503 || !strings.Contains(body, `"failing":["boom","db"]`) {
		t.Errorf("%d %s", code, body)
	}
	for _, leak := range []string{"hunter2", "10.0.0.5", "exploded"} {
		if strings.Contains(body, leak) {
			t.Errorf("health output leaks %q: %s", leak, body)
		}
	}
	ok := server.Health(map[string]func(context.Context) error{"x": func(context.Context) error { return nil }})
	rec := newRecorder()
	ok.ServeHTTP(rec, mustReq())
	if rec.code != 200 || strings.TrimSpace(rec.body.String()) != `{"status":"ok"}` {
		t.Errorf("%d %s", rec.code, rec.body.String())
	}
	rec = newRecorder()
	server.Health(nil).ServeHTTP(rec, mustReq())
	if rec.code != 200 {
		t.Errorf("no checks means healthy: %d", rec.code)
	}
}
