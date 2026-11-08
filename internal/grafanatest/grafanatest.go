// Package grafanatest is a small client for the integration tests: it creates
// data sources in a real Grafana and reads logs, traces and metrics back through
// Grafana's data source proxy, which is how a person would see them.
package grafanatest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"
)

// Grafana talks to one Grafana server as its admin user.
type Grafana struct {
	base, password string
}

// New waits for Grafana to be healthy and returns a client.
func New(t testing.TB, ctx context.Context, base, password string) *Grafana {
	t.Helper()
	g := &Grafana{strings.TrimRight(base, "/"), password}
	deadline := time.Now().Add(60 * time.Second)
	for {
		status, _, err := g.do(ctx, http.MethodGet, "/api/health", nil)
		if err == nil && status == 200 {
			return g
		}
		if time.Now().After(deadline) {
			t.Fatalf("Grafana not healthy: status %d, %v", status, err)
		}
		time.Sleep(time.Second)
	}
}

func (g *Grafana) do(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.base+path, rd)
	if err != nil {
		return 0, nil, err
	}
	req.SetBasicAuth("admin", g.password)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, nil
}

// AddDataSource creates a data source of the given type ("loki", "tempo",
// "prometheus") that Grafana reaches at targetURL (an address inside the Docker
// network), and deletes it when the test ends. It returns the data source UID.
func (g *Grafana) AddDataSource(t testing.TB, ctx context.Context, name, typ, targetURL string) string {
	t.Helper()
	status, b, err := g.do(ctx, http.MethodPost, "/api/datasources", map[string]any{
		"name": name, "type": typ, "access": "proxy", "url": targetURL,
	})
	var out struct {
		Datasource struct{ UID string } `json:"datasource"`
	}
	if err != nil || status != 200 || json.Unmarshal(b, &out) != nil || out.Datasource.UID == "" {
		t.Fatalf("create %s data source: %d %s %v", typ, status, b, err)
	}
	t.Cleanup(func() {
		g.do(context.Background(), http.MethodDelete, "/api/datasources/uid/"+out.Datasource.UID, nil)
	})
	return out.Datasource.UID
}

// Proxy sends GET path (with its query string) to the data source through Grafana.
func (g *Grafana) Proxy(ctx context.Context, uid, path string) (int, []byte, error) {
	return g.do(ctx, http.MethodGet, "/api/datasources/proxy/uid/"+uid+path, nil)
}

// Eventually calls fn every half second until it returns true or the time is up.
// Backends make fresh data queryable after a short delay.
func Eventually(timeout time.Duration, fn func() bool) bool {
	deadline := time.Now().Add(timeout)
	for {
		if fn() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// Line is one Loki log line with its stream labels.
type Line struct {
	TS     string
	Labels map[string]string
	Text   string
}

// LokiQuery runs a LogQL range query over the last ten minutes until at least
// want lines come back (or 45 seconds pass) and returns them in time order.
func (g *Grafana) LokiQuery(t testing.TB, ctx context.Context, uid, logql string, want int) []Line {
	t.Helper()
	var res []Line
	Eventually(45*time.Second, func() bool {
		q := url.Values{
			"query": {logql}, "limit": {"100"}, "direction": {"forward"},
			"start": {fmt.Sprint(time.Now().Add(-10 * time.Minute).UnixNano())},
			"end":   {fmt.Sprint(time.Now().Add(time.Minute).UnixNano())},
		}
		status, b, err := g.Proxy(ctx, uid, "/loki/api/v1/query_range?"+q.Encode())
		if err != nil {
			return false
		}
		if status >= 400 && status < 500 {
			t.Fatalf("query %s: %d %s", logql, status, b)
		}
		var body struct {
			Data struct {
				Result []struct {
					Stream map[string]string `json:"stream"`
					Values [][]any           `json:"values"`
				} `json:"result"`
			} `json:"data"`
		}
		res = nil
		if status == 200 && json.Unmarshal(b, &body) == nil {
			for _, r := range body.Data.Result {
				for _, v := range r.Values {
					res = append(res, Line{fmt.Sprint(v[0]), r.Stream, fmt.Sprint(v[1])})
				}
			}
		}
		return len(res) >= want
	})
	sort.Slice(res, func(i, j int) bool { return res[i].TS < res[j].TS })
	return res
}

// PromValue runs an instant PromQL query until it returns a sample and returns
// the first sample's value, or "" if none appeared in 45 seconds.
func (g *Grafana) PromValue(t testing.TB, ctx context.Context, uid, promql string) string {
	t.Helper()
	var val string
	Eventually(45*time.Second, func() bool {
		status, b, err := g.Proxy(ctx, uid, "/api/v1/query?query="+url.QueryEscape(promql))
		if err != nil {
			return false
		}
		if status >= 400 && status < 500 {
			t.Fatalf("query %s: %d %s", promql, status, b)
		}
		var body struct {
			Data struct {
				Result []struct {
					Value []any `json:"value"`
				} `json:"result"`
			} `json:"data"`
		}
		if status == 200 && json.Unmarshal(b, &body) == nil && len(body.Data.Result) > 0 && len(body.Data.Result[0].Value) == 2 {
			val = fmt.Sprint(body.Data.Result[0].Value[1])
			return true
		}
		return false
	})
	return val
}

// TempoSpans fetches a trace by its hex ID until Tempo has it and returns the
// span names and their service names. It returns nil if the trace never appeared.
func (g *Grafana) TempoSpans(t testing.TB, ctx context.Context, uid, traceID string) (names []string, services map[string]bool) {
	t.Helper()
	Eventually(60*time.Second, func() bool {
		status, b, err := g.Proxy(ctx, uid, "/api/traces/"+traceID)
		if err != nil || status != 200 {
			return false
		}
		var body struct {
			Batches []struct {
				Resource struct {
					Attributes []struct {
						Key   string `json:"key"`
						Value struct {
							StringValue string `json:"stringValue"`
						} `json:"value"`
					} `json:"attributes"`
				} `json:"resource"`
				ScopeSpans []struct {
					Spans []struct {
						Name string `json:"name"`
					} `json:"spans"`
				} `json:"scopeSpans"`
			} `json:"batches"`
		}
		if json.Unmarshal(b, &body) != nil || len(body.Batches) == 0 {
			return false
		}
		names, services = nil, map[string]bool{}
		for _, batch := range body.Batches {
			for _, a := range batch.Resource.Attributes {
				if a.Key == "service.name" {
					services[a.Value.StringValue] = true
				}
			}
			for _, ss := range batch.ScopeSpans {
				for _, s := range ss.Spans {
					names = append(names, s.Name)
				}
			}
		}
		return len(names) > 0
	})
	return names, services
}
