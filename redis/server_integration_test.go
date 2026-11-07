//go:build integration

package redis_test

import (
	"bytes"
	"context"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/Wetech-Code/go-common-utilities/redis"
)

// startServer starts a private, throwaway Redis server for tests that run
// destructive or disruptive commands (FLUSHALL, CONFIG SET, ACL, SHUTDOWN,
// cluster, ...), and returns its URL. It never touches a shared server.
//
// It uses the redis-server binary named by GCU_TEST_REDIS_SERVER if set (modules
// are not available then), otherwise a redis:8 Docker container (JSON, Search,
// TimeSeries and Bloom modules included). It skips the test if neither exists.
func startServer(t *testing.T, args ...string) string {
	t.Helper()
	if bin := os.Getenv("GCU_TEST_REDIS_SERVER"); bin != "" {
		return startLocal(t, bin, args)
	}
	if _, err := exec.LookPath("docker"); err == nil {
		return startDocker(t, args)
	}
	t.Skip("no disposable server: set GCU_TEST_REDIS_SERVER to a redis-server binary or install Docker")
	return ""
}

// startModuleServer starts a redis:8 container, which bundles the JSON, Search,
// TimeSeries and Bloom-family modules and the vector-set and array types that a
// plain redis-server binary may lack. It skips the test without a working Docker.
func startModuleServer(t *testing.T, args ...string) string {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil || exec.Command("docker", "info").Run() != nil {
		t.Skip("needs a running Docker for the redis:8 image (the module commands are not in a plain redis-server)")
	}
	return startDocker(t, args)
}

func startLocal(t *testing.T, bin string, args []string) string {
	t.Helper()
	port := freePort(t)
	for _, a := range args {
		if a == "--cluster-enabled" {
			port = freePortBelow(t, 45000) // the cluster bus uses port+10000, which must stay valid
		}
	}
	all := append([]string{"--port", port, "--bind", "127.0.0.1", "--save", "", "--appendonly", "no", "--dir", t.TempDir()}, args...)
	cmd := exec.Command(bin, all...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", bin, err)
	}
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		cmd.Process.Kill()
		<-done
	})
	url := "redis://127.0.0.1:" + port + "/0"
	waitReady(t, url, func() string { return out.String() })
	return url
}

func startDocker(t *testing.T, args []string, dockerArgs ...string) string {
	t.Helper()
	cmdArgs := append([]string{"run", "-d", "--rm", "-p", "127.0.0.1::6379"}, dockerArgs...)
	cmdArgs = append(cmdArgs, "redis:8", "redis-server", "--save", "")
	cmdArgs = append(cmdArgs, args...)
	id, err := exec.Command("docker", cmdArgs...).Output()
	if err != nil {
		t.Fatalf("docker run: %v", err)
	}
	cid := strings.TrimSpace(string(id))
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", cid).Run() })
	mapped, err := exec.Command("docker", "port", cid, "6379/tcp").Output()
	if err != nil {
		t.Fatalf("docker port: %v", err)
	}
	addr := strings.TrimSpace(strings.Split(string(mapped), "\n")[0]) // 127.0.0.1:49153
	url := "redis://" + addr + "/0"
	waitReady(t, url, func() string {
		l, _ := exec.Command("docker", "logs", cid).CombinedOutput()
		return string(l)
	})
	return url
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	_, port, _ := net.SplitHostPort(l.Addr().String())
	return port
}

// freePortBelow returns a free port in 20000..max.
func freePortBelow(t *testing.T, max int) string {
	t.Helper()
	for i := 0; i < 200; i++ {
		p := 20000 + rand.IntN(max-20000)
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err != nil {
			continue
		}
		l2, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p+10000))
		l.Close()
		if err != nil {
			continue
		}
		l2.Close()
		return fmt.Sprint(p)
	}
	t.Fatal("no free port for a cluster node")
	return ""
}

func waitReady(t *testing.T, url string, logs func() string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		rdb, err := redis.New(ctx, redis.Config{URL: url, DialTimeout: time.Second})
		cancel()
		if err == nil {
			rdb.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not become ready: %v\n%s", err, fmt.Sprint(logs()))
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// newClient connects a wrapped client to url and closes it when the test ends.
func newClient(t *testing.T, url string) (context.Context, *redis.Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	rdb, err := redis.New(ctx, redis.Config{URL: url})
	if err != nil {
		t.Fatal(err)
	}
	c := redis.Wrap(rdb)
	t.Cleanup(func() { c.Close() })
	return ctx, c
}

// smoke runs one named case per wrapper method so a failure names the method.
type smoke struct {
	name string
	run  func() error
}

func runSmoke(t *testing.T, cases []smoke) {
	t.Helper()
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.run(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// startPair starts a source and a target server that can reach each other, for
// MIGRATE. It returns the source URL and the host and port the source must use to
// reach the target. (A server cannot migrate to itself: it would wait on itself.)
func startPair(t *testing.T, args ...string) (srcURL, tgtHost, tgtPort string) {
	t.Helper()
	if bin := os.Getenv("GCU_TEST_REDIS_SERVER"); bin != "" {
		tgt := startLocal(t, bin, nil)
		return startLocal(t, bin, args), "127.0.0.1", portOf(t, tgt)
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("no disposable server: set GCU_TEST_REDIS_SERVER or install Docker")
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	network, target := "gcu-net-"+suffix, "gcu-target-"+suffix
	if out, err := exec.Command("docker", "network", "create", network).CombinedOutput(); err != nil {
		t.Fatalf("docker network create: %v: %s", err, out)
	}
	t.Cleanup(func() { exec.Command("docker", "network", "rm", network).Run() })
	id, err := exec.Command("docker", "run", "-d", "--rm", "--network", network, "--name", target,
		"redis:8", "redis-server", "--save", "").Output()
	if err != nil {
		t.Fatalf("docker run target: %v", err)
	}
	cid := strings.TrimSpace(string(id))
	// Registered after the network removal, so the container is removed first.
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", cid).Run() })
	src := startDocker(t, args, "--network", network)
	return src, target, "6379"
}
