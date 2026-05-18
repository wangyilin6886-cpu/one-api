package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestTopupClient_HMACSigning verifies that the wire format produced by
// TopupClient matches one-api's middleware.InternalAuth expectation EXACTLY.
// If this test ever breaks, the two sides will fall out of sync and every
// topup webhook will get a 401 from one-api.
func TestTopupClient_HMACSigning(t *testing.T) {
	const secret = "test-secret-must-be-at-least-32-bytes!"

	var capturedTS, capturedSig string
	var capturedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedTS = r.Header.Get("X-Internal-Timestamp")
		capturedSig = r.Header.Get("X-Internal-Signature")
		capturedBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"success":true,"data":{"order_no":"X","action":"topup","user_id":1,"quota":100,"new_balance":100,"idempotent_replay":false}}`))
	}))
	defer srv.Close()

	c := NewTopupClient(srv.URL, secret, 5*time.Second)
	req := TopupRequest{
		Action: "topup", OrderNo: "IDRTESTORDER0001",
		UserId: 42, Quota: 1234, Remark: "unit test",
	}
	res, err := c.Call(context.Background(), req)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if res.HTTPStatus != 200 || !res.Parsed.Success {
		t.Fatalf("response: %+v", res)
	}

	// Recompute the canonical and signature exactly as the server-side
	// middleware would. They must match.
	wantBody, _ := json.Marshal(req)
	if string(capturedBody) != string(wantBody) {
		t.Fatalf("body mismatch:\n got %q\nwant %q", capturedBody, wantBody)
	}
	tsInt, err := strconv.ParseInt(capturedTS, 10, 64)
	if err != nil {
		t.Fatalf("ts not int: %q", capturedTS)
	}
	skew := time.Now().Unix() - tsInt
	if skew < -5 || skew > 5 {
		t.Fatalf("timestamp skew suspicious: %d", skew)
	}

	bodyHash := sha256.Sum256(wantBody)
	canonical := capturedTS + "\nPOST\n/api/internal/topup\n" + hex.EncodeToString(bodyHash[:])
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(canonical))
	expectedSig := hex.EncodeToString(mac.Sum(nil))
	if capturedSig != expectedSig {
		t.Fatalf("signature mismatch:\n got %s\nwant %s", capturedSig, expectedSig)
	}
}

func TestTopupClient_NetworkError(t *testing.T) {
	// Pointing at a closed socket.
	c := NewTopupClient("http://127.0.0.1:1", "padding-padding-padding-padding-padding!", 1*time.Second)
	_, err := c.Call(context.Background(), TopupRequest{Action: "topup", OrderNo: "X", UserId: 1, Quota: 1})
	if err == nil || !strings.Contains(err.Error(), "transport") {
		t.Fatalf("want transport error, got %v", err)
	}
}

func TestTopupClient_NonSuccessHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"success":false,"code":"insufficient_balance","message":"nope"}`))
	}))
	defer srv.Close()
	c := NewTopupClient(srv.URL, "padding-padding-padding-padding-padding!", 5*time.Second)
	res, err := c.Call(context.Background(), TopupRequest{Action: "refund", OrderNo: "X", UserId: 1, Quota: 1})
	if err != nil {
		t.Fatalf("transport err: %v", err)
	}
	if res.HTTPStatus != 409 {
		t.Fatalf("status: %d", res.HTTPStatus)
	}
	if res.Parsed == nil || res.Parsed.Success {
		t.Fatalf("parsed: %+v", res.Parsed)
	}
	if res.Parsed.Code != "insufficient_balance" {
		t.Fatalf("code: %s", res.Parsed.Code)
	}
}
