package validator_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/Wetech-Code/go-common-utilities/validator"
)

type UpdateOrderRequest struct {
	OrderID string  `path:"orderId" validate:"required,uuid"`
	Status  string  `body:"status" validate:"required,oneof=pending shipped delivered" normalize:"trim,lower"`
	Note    *string `body:"note" validate:"max=500"`
	Notify  bool    `query:"notify"`
	UserID  string  `header:"X-User-ID" validate:"required,identifier"`
}

// A handler binds the whole request in one call and maps failures to a response.
func Example() {
	mux := http.NewServeMux()
	mux.HandleFunc("PATCH /orders/{orderId}", func(w http.ResponseWriter, r *http.Request) {
		var req UpdateOrderRequest
		if err := validator.Bind(r, &req); err != nil {
			var verr *validator.Errors
			if errors.As(err, &verr) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(verr.StatusCode())
				w.Write(verr.JSON())
				return
			}
			http.Error(w, "internal error", http.StatusInternalServerError) // a mistake in the struct
			return
		}
		fmt.Fprintf(w, "order %s -> %s (notify=%v, by %s)", req.OrderID, req.Status, req.Notify, req.UserID)
	})

	call := func(body string) {
		r := httptest.NewRequest("PATCH", "/orders/3f2b8c1e-5a4d-4e8b-9c1a-7d6e5f4a3b2c?notify=true", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-User-ID", "u-42") // set by your trusted ingress, not the client
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		fmt.Println(w.Code, w.Body.String())
	}
	call(`{"status":" Delivered "}`)
	call(`{"status":"finished"}`)
	// Output:
	// 200 order 3f2b8c1e-5a4d-4e8b-9c1a-7d6e5f4a3b2c -> delivered (notify=true, by u-42)
	// 400 {"errors":[{"source":"body","field":"status","code":"oneof","message":"must be one of: pending, shipped, delivered"}]}
}
