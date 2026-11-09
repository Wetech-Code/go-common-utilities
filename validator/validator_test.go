package validator_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wetech-Code/go-common-utilities/validator"
)

const uuid1 = "3f2b8c1e-5a4d-4e8b-9c1a-7d6e5f4a3b2c"

type req struct {
	method, url, body string
	header            http.Header
	path              map[string]string
}

func (q req) build() *http.Request {
	method := q.method
	if method == "" {
		method = http.MethodPost
	}
	var r *http.Request
	if q.body == "" {
		r = httptest.NewRequest(method, q.url, nil)
	} else {
		r = httptest.NewRequest(method, q.url, strings.NewReader(q.body))
		if r.Header.Get("Content-Type") == "" && q.header.Get("Content-Type") == "" {
			r.Header.Set("Content-Type", "application/json")
		}
	}
	for k, vs := range q.header {
		for i, v := range vs {
			if i == 0 {
				r.Header.Set(k, v)
			} else {
				r.Header.Add(k, v)
			}
		}
	}
	for k, v := range q.path {
		r.SetPathValue(k, v)
	}
	return r
}

func hdr(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i < len(kv); i += 2 {
		h.Add(kv[i], kv[i+1])
	}
	return h
}

type updateOrder struct {
	OrderID  string  `path:"orderId" validate:"required,uuid"`
	Status   string  `body:"status" validate:"required,oneof=pending shipped delivered" normalize:"trim,lower"`
	Note     *string `body:"note" validate:"max=20"`
	Notify   bool    `query:"notify"`
	Page     int     `query:"page" validate:"min=1,max=100" default:"1"`
	UserID   string  `header:"X-User-ID" validate:"required,identifier"`
	TenantID string  `header:"X-Tenant-ID" validate:"required,identifier"`
	Key      string  `header:"X-Idempotency-Key" validate:"max=64"`
}

func okReq() req {
	return req{
		url: "/orders/" + uuid1 + "?notify=true&page=3", body: `{"status":"  DELIVERED ","note":"all good"}`,
		header: hdr("X-User-ID", "u-42", "X-Tenant-ID", "t_7", "X-Idempotency-Key", "k-1"),
		path:   map[string]string{"orderId": uuid1},
	}
}

func bindErr(t *testing.T, r *http.Request, dst any, opts ...validator.Option) *validator.Errors {
	t.Helper()
	v, err := validator.New(opts...)
	if err != nil {
		t.Fatal(err)
	}
	err = v.Bind(r, dst)
	var verr *validator.Errors
	if !errors.As(err, &verr) {
		t.Fatalf("want *validator.Errors, got %v", err)
	}
	return verr
}

func expect(t *testing.T, e *validator.Errors, src validator.Source, name, code string) {
	t.Helper()
	for _, f := range e.Fields {
		if f.Source == src && f.Name == name && f.Code == code {
			return
		}
	}
	t.Errorf("missing %s %s %q in: %v", src, name, code, e)
}

func TestAllFourSourcesInOneStruct(t *testing.T) {
	var got updateOrder
	if err := validator.Bind(okReq().build(), &got); err != nil {
		t.Fatal(err)
	}
	note := "all good"
	want := updateOrder{OrderID: uuid1, Status: "delivered", Note: &note, Notify: true, Page: 3, UserID: "u-42", TenantID: "t_7", Key: "k-1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}

	// Optional things: no note stays nil, no page uses the default, notify defaults to false.
	r := okReq()
	r.url = "/orders/" + uuid1
	r.body = `{"status":"pending"}`
	got = updateOrder{}
	if err := validator.Bind(r.build(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Note != nil || got.Page != 1 || got.Notify || got.Status != "pending" {
		t.Errorf("optional fields: %+v", got)
	}
}

func TestParseReturnsATypedValue(t *testing.T) {
	got, err := validator.Parse[updateOrder](okReq().build())
	if err != nil || got.OrderID != uuid1 {
		t.Fatalf("Parse = %+v, %v", got, err)
	}
	bad := okReq()
	bad.body = `{"status":"nope"}`
	zero, err := validator.Parse[updateOrder](bad.build())
	if err == nil || !reflect.DeepEqual(zero, updateOrder{}) {
		t.Errorf("a failed Parse must return the zero value: %+v, %v", zero, err)
	}
	v, _ := validator.New()
	if _, err := validator.ParseWith[updateOrder](v, okReq().build()); err != nil {
		t.Error(err)
	}
}

func TestEveryProblemIsReportedAndDstIsUntouched(t *testing.T) {
	r := req{url: "/x?page=0&notify=maybe", body: `{"status":"nope","note":"this note is far too long to accept"}`,
		header: hdr("X-User-ID", "bad id!"), path: map[string]string{"orderId": "not-a-uuid"}}
	sentinel := updateOrder{OrderID: "keep-me", Page: 99}
	dst := sentinel
	e := bindErr(t, r.build(), &dst)
	for _, c := range []struct {
		src        validator.Source
		name, code string
	}{
		{validator.Path, "orderId", validator.CodeFormat},
		{validator.Body, "status", validator.CodeOneOf},
		{validator.Body, "note", validator.CodeMax},
		{validator.Query, "notify", validator.CodeType},
		{validator.Query, "page", validator.CodeMin},
		{validator.Header, "X-User-ID", validator.CodeFormat},
		{validator.Header, "X-Tenant-ID", validator.CodeRequired},
	} {
		expect(t, e, c.src, c.name, c.code)
	}
	if !reflect.DeepEqual(dst, sentinel) {
		t.Errorf("dst was modified by a failed Bind: %+v", dst)
	}
	if e.StatusCode() != 400 {
		t.Errorf("status = %d", e.StatusCode())
	}
}

func TestErrorsShapeAndNoValueLeaks(t *testing.T) {
	const s1, s2, s3, s4 = "s3cr3t-body", "s3cr3t-query", "s3cr3t-header", "s3cr3t-path"
	type dto struct {
		A int    `body:"a"`
		B int    `query:"b"`
		C int    `header:"X-C"`
		D int    `path:"d"`
		E string `body:"e" validate:"oneof=x y"`
		F string `query:"f" validate:"max=2"`
		G string `body:"g" validate:"pattern=code"`
	}
	r := req{url: "/?b=" + s2 + "&f=" + s2, body: `{"a":"` + s1 + `","e":"` + s1 + `","g":"` + s1 + `"}`,
		header: hdr("X-C", s3), path: map[string]string{"d": s4}}
	e := bindErr(t, r.build(), &dto{}, validator.Pattern("code", regexp.MustCompile(`^[A-Z]{3}$`)))
	if len(e.Fields) < 7 {
		t.Fatalf("errors = %v", e)
	}
	for _, leak := range []string{s1, s2, s3, s4} {
		if strings.Contains(e.Error(), leak) || strings.Contains(string(e.JSON()), leak) {
			t.Errorf("the rejected value %q appears in the error: %v", leak, e)
		}
	}
	js := string(e.JSON())
	if !strings.HasPrefix(js, `{"errors":[{"source":"`) || !strings.Contains(js, `"code":"type"`) {
		t.Errorf("JSON = %s", js)
	}
	if !e.Has(validator.Body, "a") || e.Has(validator.Body, "nope") {
		t.Error("Has")
	}
}

func TestBodyIsStrict(t *testing.T) {
	type dto struct {
		Name  string `body:"name" validate:"required"`
		Count int    `body:"count"`
	}
	for name, tc := range map[string]struct {
		body string
		code string
		on   string
	}{
		"malformed":          {`{"name":`, validator.CodeMalformedJSON, "body"},
		"not an object":      {`["a"]`, validator.CodeMalformedJSON, "body"},
		"a string":           {`"hi"`, validator.CodeMalformedJSON, "body"},
		"trailing data":      {`{"name":"a"} {"name":"b"}`, validator.CodeMalformedJSON, "body"},
		"trailing garbage":   {`{"name":"a"}x`, validator.CodeMalformedJSON, "body"},
		"duplicate key":      {`{"name":"a","name":"b"}`, validator.CodeDuplicate, "body"},
		"duplicate nested":   {`{"name":"a","count":1,"extra":{"k":1,"k":2}}`, validator.CodeDuplicate, "body"},
		"duplicate in array": {`{"name":"a","x":[{"k":1,"k":2}]}`, validator.CodeDuplicate, "body"},
		"unknown field":      {`{"name":"a","admin":true}`, validator.CodeUnknownField, "admin"},
		"wrong case":         {`{"Name":"a"}`, validator.CodeUnknownField, "Name"},
		"string for int":     {`{"name":"a","count":"5"}`, validator.CodeType, "count"},
		"float for int":      {`{"name":"a","count":1.5}`, validator.CodeType, "count"},
		"huge int":           {`{"name":"a","count":99999999999999999999}`, validator.CodeType, "count"},
		"number for string":  {`{"name":5}`, validator.CodeType, "name"},
		"null is missing":    {`{"name":null}`, validator.CodeRequired, "name"},
		"empty string":       {`{"name":""}`, validator.CodeRequired, "name"},
		"bad utf8":           {"{\"name\":\"a\xff\"}", validator.CodeMalformedJSON, "body"},
		"nul byte":           {"{\"name\":\"a\\u0000b\"}", validator.CodeControl, "name"},
	} {
		e := bindErr(t, req{url: "/", body: tc.body}.build(), &dto{})
		found := false
		for _, f := range e.Fields {
			if f.Code == tc.code && f.Name == tc.on {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: want %s on %q, got %v", name, tc.code, tc.on, e)
		}
	}

	// A body that is rejected as a whole does not also produce "required" noise.
	e := bindErr(t, req{url: "/", body: `{"name":`}.build(), &dto{})
	if len(e.Fields) != 1 {
		t.Errorf("one error expected for a malformed body, got %v", e)
	}

	// Allowed: unknown keys with the option; empty body with no required body fields.
	var d dto
	v, _ := validator.New(validator.AllowUnknownBodyFields())
	if err := v.Bind(req{url: "/", body: `{"name":"a","whatever":{"x":1}}`}.build(), &d); err != nil || d.Name != "a" {
		t.Errorf("unknown fields allowed: %+v %v", d, err)
	}
	type optional struct {
		Note string `body:"note"`
	}
	var o optional
	if err := validator.Bind(httptest.NewRequest("GET", "/", nil), &o); err != nil {
		t.Errorf("a request without a body and no required fields: %v", err)
	}
}

func TestBodySizeDepthAndContentType(t *testing.T) {
	type dto struct {
		Note string `body:"note"`
	}
	e := bindErr(t, req{url: "/", body: `{"note":"` + strings.Repeat("x", 100) + `"}`}.build(), &dto{}, validator.MaxBodySize(50))
	if e.StatusCode() != http.StatusRequestEntityTooLarge || !e.Has(validator.Request, "body") {
		t.Errorf("too large: %v (%d)", e, e.StatusCode())
	}
	deep := `{"note":"a","x":` + strings.Repeat("[", 40) + strings.Repeat("]", 40) + `}`
	e = bindErr(t, req{url: "/", body: deep}.build(), &dto{}, validator.AllowUnknownBodyFields())
	expect(t, e, validator.Request, "body", validator.CodeTooDeep)
	// 31 levels (plus the top-level object) is within the default limit of 32.
	ok := `{"note":"a","x":` + strings.Repeat("[", 31) + strings.Repeat("]", 31) + `}`
	if err := validator.Bind(req{url: "/", body: ok}.build(), &dto{}); err == nil {
		t.Log("(unknown field x rejected as expected)")
	}
	v, _ := validator.New(validator.AllowUnknownBodyFields())
	if err := v.Bind(req{url: "/", body: ok}.build(), &dto{}); err != nil {
		t.Errorf("nesting within the limit: %v", err)
	}

	for name, tc := range map[string]struct {
		h    http.Header
		want int
		code string
	}{
		"text/plain":        {hdr("Content-Type", "text/plain"), 415, validator.CodeMediaType},
		"form":              {hdr("Content-Type", "application/x-www-form-urlencoded"), 415, validator.CodeMediaType},
		"latin1 charset":    {hdr("Content-Type", "application/json; charset=iso-8859-1"), 415, validator.CodeMediaType},
		"garbage type":      {hdr("Content-Type", ";;;"), 415, validator.CodeMediaType},
		"duplicate headers": {hdr("Content-Type", "application/json", "Content-Type", "application/json"), 400, validator.CodeDuplicate},
	} {
		r := req{url: "/", body: `{"note":"a"}`, header: tc.h}.build()
		e := bindErr(t, r, &dto{})
		if e.StatusCode() != tc.want || !e.Has(validator.Header, "Content-Type") {
			t.Errorf("%s: %v (%d)", name, e, e.StatusCode())
		}
	}
	// Missing content type with a body is refused; JSON with parameters and +json types are accepted.
	r := httptest.NewRequest("POST", "/", strings.NewReader(`{"note":"a"}`))
	if e := bindErr(t, r, &dto{}); e.StatusCode() != 415 {
		t.Errorf("missing content type: %v", e)
	}
	for _, ct := range []string{"application/json; charset=utf-8", "APPLICATION/JSON", "application/merge-patch+json"} {
		var d dto
		if err := validator.Bind(req{url: "/", body: `{"note":"a"}`, header: hdr("Content-Type", ct)}.build(), &d); err != nil || d.Note != "a" {
			t.Errorf("%q: %v", ct, err)
		}
	}
}

func TestQueryHandling(t *testing.T) {
	type dto struct {
		Tags  []string      `query:"tag" validate:"max=3" each:"identifier"`
		IDs   []int         `query:"id"`
		On    bool          `query:"on"`
		Limit *int          `query:"limit"`
		Since time.Time     `query:"since"`
		Wait  time.Duration `query:"wait" validate:"max=1m"`
		Name  string        `query:"name" validate:"required"`
	}
	var d dto
	if err := validator.Bind(req{url: "/?tag=a&tag=b&id=1&id=2&on=TRUE&limit=7&since=2026-11-09T10:00:00Z&wait=30s&name=x"}.build(), &d); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(d.Tags, []string{"a", "b"}) || !reflect.DeepEqual(d.IDs, []int{1, 2}) || !d.On || *d.Limit != 7 ||
		d.Since.Year() != 2026 || d.Wait != 30*time.Second {
		t.Errorf("d = %+v", d)
	}

	for name, tc := range map[string]struct {
		query string
		field string
		code  string
	}{
		"repeated scalar":   {"name=a&name=b", "name", validator.CodeDuplicate},
		"empty is missing":  {"name=", "name", validator.CodeRequired},
		"bool yes":          {"name=x&on=yes", "on", validator.CodeType},
		"bool one":          {"name=x&on=1", "on", validator.CodeType},
		"plus int":          {"name=x&limit=%2B5", "limit", validator.CodeType},
		"float for int":     {"name=x&limit=1.5", "limit", validator.CodeType},
		"overflow":          {"name=x&limit=99999999999999999999", "limit", validator.CodeType},
		"bad time":          {"name=x&since=yesterday", "since", validator.CodeType},
		"bad duration":      {"name=x&wait=soon", "wait", validator.CodeType},
		"duration too long": {"name=x&wait=2h", "wait", validator.CodeMax},
		"too many tags":     {"name=x&tag=a&tag=b&tag=c&tag=d", "tag", validator.CodeMax},
		"bad tag element":   {"name=x&tag=ok&tag=has+space", "tag[1]", validator.CodeFormat},
		"bad slice element": {"name=x&id=1&id=two", "id", validator.CodeType},
		"control character": {"name=a%0Ab", "name", validator.CodeControl},
		"nul":               {"name=a%00b", "name", validator.CodeControl},
	} {
		e := bindErr(t, req{url: "/?" + tc.query}.build(), &dto{})
		expect(t, e, validator.Query, tc.field, tc.code)
		_ = name
	}

	// Malformed escapes and semicolons are rejected, not half-read.
	for _, q := range []string{"name=%zz", "name=a;b=c", "name=%"} {
		e := bindErr(t, req{url: "/?" + q}.build(), &dto{})
		expect(t, e, validator.Query, "", validator.CodeFormat)
	}
	// Too many parameters, and unknown ones when asked to reject them.
	many := "name=x"
	for i := 0; i < 120; i++ {
		many += fmt.Sprintf("&p%d=1", i)
	}
	expect(t, bindErr(t, req{url: "/?" + many}.build(), &dto{}), validator.Query, "", validator.CodeTooMany)
	if err := validator.Bind(req{url: "/?name=x&utm_source=mail"}.build(), &dto{}); err != nil {
		t.Errorf("unknown query parameters are ignored by default: %v", err)
	}
	e := bindErr(t, req{url: "/?name=x&utm_source=mail&a%00=1"}.build(), &dto{}, validator.RejectUnknownQuery())
	expect(t, e, validator.Query, "utm_source", validator.CodeUnknownQueryKey)
	expect(t, e, validator.Query, "?", validator.CodeUnknownQueryKey)

	// A very long value is refused before it is parsed.
	e = bindErr(t, req{url: "/?name=" + strings.Repeat("a", 200)}.build(), &dto{}, validator.MaxValueSize(100))
	expect(t, e, validator.Query, "name", validator.CodeMax)

	// Floats: only plain decimal numbers.
	type fl struct {
		F float64 `query:"f"`
	}
	for _, bad := range []string{"NaN", "Inf", "-inf", "0x1p-2", "1_0", "1e999", "abc"} {
		expect(t, bindErr(t, req{url: "/?f=" + bad}.build(), &fl{}), validator.Query, "f", validator.CodeType)
	}
	var f fl
	if err := validator.Bind(req{url: "/?f=-12.5e1"}.build(), &f); err != nil || f.F != -125 {
		t.Errorf("float = %v, %v", f, err)
	}
}

func TestHeaderHandling(t *testing.T) {
	type dto struct {
		User   string   `header:"x-user-id" validate:"required"`
		Accept []string `header:"X-Flags"`
	}
	var d dto
	if err := validator.Bind(req{url: "/", header: hdr("X-User-Id", "u1", "X-Flags", "a", "X-Flags", "b")}.build(), &d); err != nil {
		t.Fatal(err)
	}
	if d.User != "u1" || !reflect.DeepEqual(d.Accept, []string{"a", "b"}) {
		t.Errorf("d = %+v", d)
	}
	// A repeated identity header is ambiguous: refused rather than picking one.
	e := bindErr(t, req{url: "/", header: hdr("X-User-ID", "alice", "X-User-ID", "admin")}.build(), &dto{})
	expect(t, e, validator.Header, "x-user-id", validator.CodeDuplicate)
	// Control characters in a header value.
	r := req{url: "/"}.build()
	r.Header["X-User-Id"] = []string{"a\x00b"}
	expect(t, bindErr(t, r, &dto{}), validator.Header, "x-user-id", validator.CodeControl)
	// Header names are canonicalized, and a missing required header is reported.
	expect(t, bindErr(t, req{url: "/"}.build(), &dto{}), validator.Header, "x-user-id", validator.CodeRequired)
}

func TestPathValues(t *testing.T) {
	type dto struct {
		ID  int     `path:"id" validate:"min=1"`
		Opt *string `path:"opt"`
	}
	var d dto
	if err := validator.Bind(req{url: "/", path: map[string]string{"id": "42"}}.build(), &d); err != nil || d.ID != 42 || d.Opt != nil {
		t.Errorf("d = %+v, %v", d, err)
	}
	expect(t, bindErr(t, req{url: "/", path: map[string]string{"id": "x"}}.build(), &dto{}), validator.Path, "id", validator.CodeType)
	expect(t, bindErr(t, req{url: "/", path: map[string]string{"id": "0"}}.build(), &dto{}), validator.Path, "id", validator.CodeMin)

	// Another router's way of reading path values.
	fake := func(r *http.Request, name string) string { return r.Header.Get("X-Route-" + name) }
	var d2 dto
	v, _ := validator.New(validator.PathValue(fake))
	r := req{url: "/", header: hdr("X-Route-id", "7")}.build()
	if err := v.Bind(r, &d2); err != nil || d2.ID != 7 {
		t.Errorf("custom PathValue: %+v, %v", d2, err)
	}
}

func TestRules(t *testing.T) {
	type item struct {
		SKU   string `json:"sku" validate:"required,pattern=sku"`
		Qty   *int   `json:"qty" validate:"required,min=1,max=100"`
		Unit  string `json:"unit" validate:"oneof=ml l kg"`
		Notes string `json:"notes"`
	}
	type dto struct {
		Name   string            `body:"name" validate:"min=2,max=5"`
		Code   string            `body:"code" validate:"len=3,alphanum"`
		Site   string            `body:"site" validate:"url"`
		Level  int               `body:"level" validate:"oneof=1 2 3"`
		Age    uint8             `body:"age" validate:"min=18,max=99"`
		Ratio  float64           `body:"ratio" validate:"min=0.5,max=1.5"`
		TTL    time.Duration     `body:"ttl" validate:"min=1s"`
		Labels map[string]string `body:"labels" validate:"max=2"`
		Even   int               `body:"even" validate:"even"`
		Items  []item            `body:"items" validate:"min=1,max=2,dive"`
		Single *item             `body:"single" validate:"dive"`
		Words  []string          `body:"words" each:"max=3,alphanum"`
	}
	opts := []validator.Option{
		validator.Pattern("sku", regexp.MustCompile(`^[A-Z]{2}-\d{3}$`)),
		validator.Rule("even", func(v any, _ string) error {
			if n, ok := v.(int); ok && n%2 != 0 {
				return errors.New("must be even")
			}
			return nil
		}),
	}
	good := `{"name":"héllo","code":"ab1","site":"https://example.com/x","level":2,"age":30,"ratio":1,"ttl":"2s"` +
		`,"labels":{"a":"b"},"even":4,"items":[{"sku":"AB-123","qty":5,"unit":"kg"}],"single":{"sku":"CD-456","qty":1},"words":["ok","yes"]}`
	v, _ := validator.New(opts...)
	var d dto
	if err := v.Bind(req{url: "/", body: good}.build(), &d); err != nil {
		t.Fatalf("a valid body was rejected: %v", err)
	}
	if d.Name != "héllo" || len(d.Items) != 1 || *d.Items[0].Qty != 5 || d.Single.SKU != "CD-456" {
		t.Errorf("d = %+v", d)
	}

	bad := `{"name":"x","code":"abcd","site":"ftp://x","level":4,"age":17,"ratio":2,"ttl":"1ms","labels":{"a":"1","b":"2","c":"3"},"even":3,` +
		`"items":[{"sku":"nope","qty":0,"unit":"pt"},{"qty":500},{"sku":"EF-789","qty":2}],"single":{"sku":""},"words":["toolong","ok!"]}`
	e := bindErr(t, req{url: "/", body: bad}.build(), &dto{}, opts...)
	for _, c := range []struct{ name, code string }{
		{"name", validator.CodeMin}, {"code", validator.CodeLength}, {"site", validator.CodeFormat}, {"level", validator.CodeOneOf},
		{"age", validator.CodeMin}, {"ratio", validator.CodeMax}, {"ttl", validator.CodeMin}, {"labels", validator.CodeMax}, {"even", validator.CodeRule},
		{"items", validator.CodeMax},
		{"items[0].sku", validator.CodePattern}, {"items[0].qty", validator.CodeMin}, {"items[0].unit", validator.CodeOneOf},
		{"items[1].sku", validator.CodeRequired}, {"items[1].qty", validator.CodeMax},
		{"single.sku", validator.CodeRequired}, {"single.qty", validator.CodeRequired},
		{"words[0]", validator.CodeMax}, {"words[1]", validator.CodeFormat},
	} {
		expect(t, e, validator.Body, c.name, c.code)
	}

	// min/max count characters, not bytes.
	e = bindErr(t, req{url: "/", body: `{"name":"日"}`}.build(), &dto{}, opts...)
	expect(t, e, validator.Body, "name", validator.CodeMin)
	if err := v.Bind(req{url: "/", body: `{"name":"日本"}`}.build(), &dto{}); err != nil {
		t.Errorf("two characters should pass min=2: %v", err)
	}
}

func TestBodyDurationsAreStrings(t *testing.T) {
	type dto struct {
		D  time.Duration  `body:"d"`
		DP *time.Duration `body:"dp"`
	}
	var d dto
	if err := validator.Bind(req{url: "/", body: `{"d":"1m30s","dp":"250ms"}`}.build(), &d); err != nil || d.D != 90*time.Second || *d.DP != 250*time.Millisecond {
		t.Errorf("d = %+v, %v", d, err)
	}
	for _, body := range []string{`{"d":5}`, `{"d":"soon"}`, `{"d":true}`} {
		expect(t, bindErr(t, req{url: "/", body: body}.build(), &dto{}), validator.Body, "d", validator.CodeType)
	}
}

func TestNormalizationIsExplicitAndTextHygiene(t *testing.T) {
	type dto struct {
		A string   `body:"a" normalize:"trim" validate:"required"`
		B string   `body:"b" normalize:"trim,upper"`
		C string   `body:"c"`
		D []string `body:"d" normalize:"trim,lower"`
		E string   `query:"e" normalize:"lower"`
		F string   `body:"f" validate:"allowcontrol"`
	}
	var d dto
	body := `{"a":"  keep  ","b":" abc ","c":"  untouched  ","d":[" X ","Y"],"f":"tab\there\u0001"}`
	if err := validator.Bind(req{url: "/?e=MiXed", body: body}.build(), &d); err != nil {
		t.Fatal(err)
	}
	if d.A != "keep" || d.B != "ABC" || d.C != "  untouched  " || !reflect.DeepEqual(d.D, []string{"x", "y"}) || d.E != "mixed" || !strings.Contains(d.F, "\u0001") {
		t.Errorf("d = %+v", d)
	}
	// Whitespace-only becomes empty after trim, so required fails.
	e := bindErr(t, req{url: "/", body: `{"a":"   "}`}.build(), &dto{})
	expect(t, e, validator.Body, "a", validator.CodeRequired)
	// Body text may contain newlines and tabs, but not other control characters.
	if err := validator.Bind(req{url: "/", body: `{"a":"line1\nline2\tx","c":"a\r\nb"}`}.build(), &dto{}); err != nil {
		t.Errorf("newlines in body text: %v", err)
	}
	expect(t, bindErr(t, req{url: "/", body: `{"a":"x","c":"bell\u0007"}`}.build(), &dto{}), validator.Body, "c", validator.CodeControl)
}

type color int

func (c *color) UnmarshalText(b []byte) error {
	switch string(b) {
	case "red":
		*c = 1
	case "blue":
		*c = 2
	default:
		return errors.New("unknown colour")
	}
	return nil
}

type paging struct {
	Page int `query:"page" default:"1" validate:"min=1"`
	Size int `query:"size" default:"20" validate:"min=1,max=100"`
}

func TestEmbeddedStructsAndTextUnmarshalers(t *testing.T) {
	type dto struct {
		paging
		Paging2 int    `query:"p2"`
		Color   color  `query:"color"`
		HdrCol  *color `header:"X-Color"`
	}
	_ = paging{}
	type withEmbedded struct {
		paging
		Q     string `query:"q"`
		Color color  `query:"color"`
	}
	var d withEmbedded
	if err := validator.Bind(req{url: "/?q=x&color=blue&size=50"}.build(), &d); err != nil {
		t.Fatal(err)
	}
	if d.Page != 1 || d.Size != 50 || d.Color != 2 {
		t.Errorf("d = %+v", d)
	}
	expect(t, bindErr(t, req{url: "/?color=green"}.build(), &withEmbedded{}), validator.Query, "color", validator.CodeType)
	expect(t, bindErr(t, req{url: "/?size=500"}.build(), &withEmbedded{}), validator.Query, "size", validator.CodeMax)
	var h struct {
		C *color `header:"X-Color"`
	}
	if err := validator.Bind(req{url: "/", header: hdr("X-Color", "red")}.build(), &h); err != nil || h.C == nil || *h.C != 1 {
		t.Errorf("header TextUnmarshaler: %+v %v", h, err)
	}
	_ = dto{}
}

func TestSetupMistakesAreNotBadRequests(t *testing.T) {
	r := req{url: "/"}.build()
	v, _ := validator.New()
	var notStruct int
	type okType struct {
		A string `query:"a"`
	}
	for name, dst := range map[string]any{
		"nil":                   nil,
		"not a pointer":         okType{},
		"nil pointer":           (*okType)(nil),
		"pointer to non-struct": &notStruct,
		"two sources": &struct {
			A string `query:"a" header:"X-A"`
		}{},
		"duplicate name": &struct {
			A, B string `query:"a"`
		}{},
		"unknown rule": &struct {
			A string `query:"a" validate:"nonsense"`
		}{},
		"rule needs value": &struct {
			A string `query:"a" validate:"max"`
		}{},
		"rule bad value": &struct {
			A string `query:"a" validate:"max=lots"`
		}{},
		"negative length": &struct {
			A string `query:"a" validate:"max=-1"`
		}{},
		"min on bool": &struct {
			A bool `query:"a" validate:"min=1"`
		}{},
		"len on number": &struct {
			A int `query:"a" validate:"len=2"`
		}{},
		"oneof on float": &struct {
			A float64 `query:"a" validate:"oneof=1 2"`
		}{},
		"oneof no values": &struct {
			A string `query:"a" validate:"oneof="`
		}{},
		"uuid on int": &struct {
			A int `query:"a" validate:"uuid"`
		}{},
		"pattern unregistered": &struct {
			A string `query:"a" validate:"pattern=missing"`
		}{},
		"dive on string": &struct {
			A string `body:"a" validate:"dive"`
		}{},
		"each on non-slice": &struct {
			A string `query:"a" each:"max=2"`
		}{},
		"normalize on int": &struct {
			A int `query:"a" normalize:"trim"`
		}{},
		"unknown normalize": &struct {
			A string `query:"a" normalize:"reverse"`
		}{},
		"required with default": &struct {
			A string `query:"a" validate:"required" default:"x"`
		}{},
		"default on body": &struct {
			A string `body:"a" default:"x"`
		}{},
		"bad default": &struct {
			A int `query:"a" default:"many"`
		}{},
		"map in query": &struct {
			A map[string]string `query:"a"`
		}{},
		"slice in path": &struct {
			A []string `path:"a"`
		}{},
		"chan in body": &struct {
			A chan int `body:"a"`
		}{},
		"struct in query": &struct {
			A struct{ X int } `query:"a"`
		}{},
		"tags without source": &struct {
			A string `validate:"required"`
		}{},
		"empty name": &struct {
			A string `query:""`
		}{},
		"name with comma": &struct {
			A string `query:"a,b"`
		}{},
		"nested required number": &struct {
			A []struct {
				N int `validate:"required"`
			} `body:"a" validate:"dive"`
		}{},
		"nested bad rule": &struct {
			A []struct {
				N string `validate:"bogus"`
			} `body:"a" validate:"dive"`
		}{},
		"embedded with tag": &struct {
			paging `query:"p"`
		}{},
	} {
		err := v.Bind(r, dst)
		if !errors.Is(err, validator.ErrSetup) || !validator.IsSetupError(err) {
			t.Errorf("%s: want ErrSetup, got %v", name, err)
			continue
		}
		var verr *validator.Errors
		if errors.As(err, &verr) {
			t.Errorf("%s: a setup mistake must not look like a bad request", name)
		}
	}
	if err := v.Bind(nil, &okType{}); !errors.Is(err, validator.ErrSetup) {
		t.Errorf("nil request: %v", err)
	}
	// The same mistake is reported again on the second call (cached, not forgotten).
	bad := &struct {
		A string `query:"a" validate:"nonsense"`
	}{}
	if v.Bind(r, bad) == nil || v.Bind(r, bad) == nil {
		t.Error("a setup error must be reported every time")
	}
}

func TestNewRejectsBadOptions(t *testing.T) {
	for name, opt := range map[string]validator.Option{
		"zero body":         validator.MaxBodySize(0),
		"negative value":    validator.MaxValueSize(-1),
		"zero depth":        validator.MaxJSONDepth(0),
		"nil path":          validator.PathValue(nil),
		"nil pattern":       validator.Pattern("x", nil),
		"empty pattern":     validator.Pattern("", regexp.MustCompile(`x`)),
		"builtin rule name": validator.Rule("required", func(any, string) error { return nil }),
		"empty rule name":   validator.Rule("", func(any, string) error { return nil }),
		"nil rule":          validator.Rule("custom", nil),
	} {
		if _, err := validator.New(opt); !errors.Is(err, validator.ErrSetup) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestRepeatedValuesAndTrustBoundaryDocumentedBehavior(t *testing.T) {
	// Binding a header only checks its format: a forged X-User-ID with a valid
	// shape binds fine. Whether it can be trusted is up to the ingress.
	type dto struct {
		User string `header:"X-User-ID" validate:"required,identifier"`
	}
	var d dto
	if err := validator.Bind(req{url: "/", header: hdr("X-User-ID", "admin")}.build(), &d); err != nil || d.User != "admin" {
		t.Errorf("%+v %v", d, err)
	}
}

func TestConcurrentUse(t *testing.T) {
	v, _ := validator.New()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				var d updateOrder
				r := okReq()
				r.url = fmt.Sprintf("/orders/%s?page=%d", uuid1, i%100+1)
				if err := v.Bind(r.build(), &d); err != nil || d.Page != i%100+1 {
					t.Errorf("g%d i%d: %+v %v", g, i, d, err)
					return
				}
				bad := okReq()
				bad.body = `{"status":"nope"}`
				if err := v.Bind(bad.build(), &updateOrder{}); err == nil {
					t.Error("want an error")
					return
				}
			}
		}(g)
	}
	wg.Wait()
}

func FuzzBind(f *testing.F) {
	f.Add(`{"status":"pending"}`, "page=2", "u1")
	f.Add(`{"status":"pending","status":"delivered"}`, "page=%zz", "")
	f.Add(`[1,2`, "page=0&notify=x", "a\x00b")
	f.Add(`{"status":"\u0000"}`, "", "u")
	v, _ := validator.New()
	f.Fuzz(func(t *testing.T, body, query, user string) {
		r := httptest.NewRequest("POST", "/", strings.NewReader(body))
		r.URL.RawQuery = query
		r.Header.Set("Content-Type", "application/json")
		r.Header["X-User-Id"] = []string{user}
		r.Header["X-Tenant-Id"] = []string{"o1"}
		r.SetPathValue("orderId", uuid1)
		var d updateOrder
		if err := v.Bind(r, &d); err != nil {
			return
		}
		// Anything accepted must satisfy the declared rules.
		if d.Status != "pending" && d.Status != "shipped" && d.Status != "delivered" {
			t.Fatalf("accepted an invalid status %q", d.Status)
		}
		if d.Page < 1 || d.Page > 100 || d.UserID == "" || strings.ContainsAny(d.UserID, "\x00\n ") {
			t.Fatalf("accepted invalid values: %+v", d)
		}
	})
}

type audit struct {
	By string `json:"by" validate:"required,identifier"`
}

type node struct {
	Name     string   `json:"name" validate:"required,max=5" normalize:"trim"`
	Children []node   `json:"children" validate:"max=2,dive"`
	Tags     []string `json:"tags" each:"alphanum,max=4"`
	Count    *int     `json:"count" validate:"required,min=1"`
	On       bool     `json:"on"`
	audit
}

func TestNestedValidation(t *testing.T) {
	type dto struct {
		Root node    `body:"root" validate:"dive"`
		Many []*node `body:"many" validate:"dive"`
	}
	good := `{"root":{"name":" a ","count":2,"by":"u1","tags":["ab"],"children":[{"name":"b","count":1,"by":"u2"}]}}`
	var d dto
	if err := validator.Bind(req{url: "/", body: good, header: nil}.build(), &d); err != nil {
		t.Fatal(err)
	}
	if d.Root.Name != "a" || len(d.Root.Children) != 1 {
		t.Errorf("d = %+v", d)
	}
	bad := `{"root":{"name":"toolongname","count":0,"tags":["nope!","toolong"],"children":[{"count":1},{"name":"c","count":1,"by":"x"},{"name":"d","count":1,"by":"y"}]},` +
		`"many":[{"name":"ok","count":1,"by":"z"},{"name":""}]}`
	e := bindErr(t, req{url: "/", body: bad}.build(), &dto{})
	for _, c := range []struct{ name, code string }{
		{"root.name", validator.CodeMax}, {"root.count", validator.CodeMin}, {"root.by", validator.CodeRequired},
		{"root.tags[0]", validator.CodeFormat}, {"root.tags[1]", validator.CodeMax},
		{"root.children", validator.CodeMax}, {"root.children[0].name", validator.CodeRequired}, {"root.children[0].by", validator.CodeRequired},
		{"many[1].name", validator.CodeRequired}, {"many[1].count", validator.CodeRequired},
	} {
		expect(t, e, validator.Body, c.name, c.code)
	}
	// Control characters inside nested text are refused too.
	e = bindErr(t, req{url: "/", body: `{"root":{"name":"a\u0007","count":1,"by":"u"}}`}.build(), &dto{})
	expect(t, e, validator.Body, "root.name", validator.CodeControl)
	// A nested unknown key is rejected unless unknown fields are allowed.
	e = bindErr(t, req{url: "/", body: `{"root":{"name":"a","count":1,"by":"u","admin":true}}`}.build(), &dto{})
	expect(t, e, validator.Body, "root", validator.CodeUnknownField)
}

func TestLongAndUnprintableNamesAreSafeToShowBack(t *testing.T) {
	type dto struct {
		A string `body:"a"`
	}
	long := strings.Repeat("k", 200)
	e := bindErr(t, req{url: "/", body: `{"` + long + `":1,"a\u0001b":2}`}.build(), &dto{})
	var seen []string
	for _, f := range e.Fields {
		seen = append(seen, f.Name)
		if len(f.Name) > 70 {
			t.Errorf("name not bounded: %d characters", len(f.Name))
		}
	}
	if !strings.Contains(strings.Join(seen, "|"), "...") || !strings.Contains(strings.Join(seen, "|"), "?") {
		t.Errorf("names = %q", seen)
	}
}

func TestOptionalNilsAndZeroValuesInRequiredNumbers(t *testing.T) {
	type dto struct {
		N   int  `body:"n" validate:"required"`
		B   bool `body:"b" validate:"required"`
		Opt *int `body:"opt"`
	}
	var d dto
	if err := validator.Bind(req{url: "/", body: `{"n":0,"b":false}`}.build(), &d); err != nil {
		t.Errorf("a present zero value satisfies required: %v", err)
	}
	e := bindErr(t, req{url: "/", body: `{}`}.build(), &dto{})
	expect(t, e, validator.Body, "n", validator.CodeRequired)
	expect(t, e, validator.Body, "b", validator.CodeRequired)
}

type badEnum string

func (b *badEnum) UnmarshalText(p []byte) error { return errors.New("secret detail " + string(p)) }

func TestTextUnmarshalerErrorsDoNotLeak(t *testing.T) {
	type dto struct {
		E badEnum `query:"e"`
	}
	e := bindErr(t, req{url: "/?e=hunter2"}.build(), &dto{})
	if strings.Contains(e.Error(), "hunter2") || strings.Contains(e.Error(), "secret detail") {
		t.Errorf("leak: %v", e)
	}
}

func TestKeysThatDifferOnlyByCaseAreDuplicates(t *testing.T) {
	type item struct {
		SKU string `json:"sku" validate:"required"`
	}
	type dto struct {
		Name  string `body:"name"`
		Items []item `body:"items" validate:"dive"`
	}
	for name, body := range map[string]string{
		"top level":       `{"name":"a","Name":"b"}`,
		"top level upper": `{"name":"a","NAME":"b"}`,
		"nested":          `{"items":[{"sku":"A","SKU":"B"}]}`,
		"nested deep":     `{"items":[{"sku":"A"},{"sku":"B","sKu":"C"}]}`,
		"kelvin sign":     `{"name":"a","\u212Aey":1,"key":2}`, // U+212A folds to k, as encoding/json does
		"unicode fold":    `{"name":"a","stra\u00dfe":1,"STRA\u1E9Ee":2}`,
	} {
		e := bindErr(t, req{url: "/", body: body}.build(), &dto{}, validator.AllowUnknownBodyFields())
		found := false
		for _, f := range e.Fields {
			if f.Code == validator.CodeDuplicate {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: want a duplicate error, got %v", name, e)
		}
	}
	// Different keys, even with similar letters, are fine.
	var d dto
	if err := validator.Bind(req{url: "/", body: `{"name":"a","items":[{"sku":"A"},{"sku":"B"}]}`}.build(), &d); err != nil || len(d.Items) != 2 {
		t.Errorf("%+v %v", d, err)
	}
}
