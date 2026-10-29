package main

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCheckoutBeforeSaleDoesNotEnterPersistencePath(t *testing.T) {
	s := &server{saleStart: time.Now().Add(time.Hour), productID: "phone-500"}
	r := httptest.NewRequest("POST", "/api/v1/checkout", strings.NewReader(`{"product_id":"phone-500","quantity":1}`))
	r.Header.Set("Idempotency-Key", "k1")
	r.Header.Set("X-User-ID", "u1")
	w := httptest.NewRecorder()
	s.checkout(w, r)
	if w.Code != 403 {
		t.Fatalf("expected 403 before launch, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "NOT_LIVE") {
		t.Fatalf("expected NOT_LIVE response: %s", w.Body.String())
	}
}
