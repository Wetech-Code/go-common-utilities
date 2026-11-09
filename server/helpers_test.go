package server_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
)

type recorder struct {
	*httptest.ResponseRecorder
	code int
	body *bytes.Buffer
}

func newRecorder() *recorder {
	r := httptest.NewRecorder()
	return &recorder{ResponseRecorder: r, code: 200, body: r.Body}
}

func (r *recorder) WriteHeader(c int) { r.code = c; r.ResponseRecorder.WriteHeader(c) }

func mustReq() *http.Request { return httptest.NewRequest("GET", "/healthz", nil) }
