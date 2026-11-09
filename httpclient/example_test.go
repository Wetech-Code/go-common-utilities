package httpclient_test

import (
	"context"
	"fmt"
	"net/http"

	"github.com/Wetech-Code/go-common-utilities/httpclient"
	"github.com/Wetech-Code/go-common-utilities/reqctx"
)

// Call another service through a gateway. The context carries the request ID, a header is
// forwarded only because it is named explicitly, and a write is retried only because it
// has an idempotency key.
func Example() {
	api, err := httpclient.New(httpclient.Config{
		BaseURL:    "https://gateway.internal",
		PathPrefix: "/api",
		Headers:    map[string]string{"X-Caller": "orders"},
		// HTTPClient: &http.Client{Transport: mutualTLSTransport}, // how callers authenticate is your choice
	})
	if err != nil {
		fmt.Println(err)
		return
	}
	payments, err := api.WithPrefix("payments") // URLs become /api/payments/...
	if err != nil {
		fmt.Println(err)
		return
	}
	ctx := reqctx.WithRequestID(context.Background(), "req-123")
	ctx = reqctx.WithHeaders(ctx, map[string]string{"X-Tenant-ID": "t-7"}) // normally set by middleware.CaptureHeaders

	var receipt struct{ ID string }
	err = payments.JSON(ctx, http.MethodPost, httpclient.Path("charges", "ch-9", "capture"), nil, &receipt,
		httpclient.ForwardFromContext("X-Tenant-ID"), httpclient.IdempotencyKey("order-5-capture"))
	if err != nil {
		fmt.Println("could not capture:", err) // errors.Is(err, httpclient.ErrTimeout / ErrUnavailable / ErrStatus)
	}
}
