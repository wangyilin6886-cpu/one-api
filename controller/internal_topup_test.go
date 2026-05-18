package controller

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/model"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// setupTestDB creates a file-backed SQLite database (shared by readers/writers
// in the same process) and migrates the schema. We use a temp file rather than
// :memory: because the latter creates a fresh per-connection schema, which
// breaks SELECT FOR UPDATE-style transactions across the connection pool.
func setupTestDB(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	db, err := gorm.Open(sqlite.Open(dbPath+"?_busy_timeout=5000"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.InternalTopupRecord{}, &model.Log{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	model.DB = db
	model.LOG_DB = db
	common.UsingSQLite = true
	// Tests do not run a Redis server. RedisEnabled defaults to true (see
	// common/redis.go), so we have to flip it off or CacheUpdateUserQuota
	// will try to dereference a nil RDB.
	common.RedisEnabled = false
}

func seedUser(t *testing.T, id int, quota int64) {
	t.Helper()
	u := &model.User{
		Id:       id,
		Username: "test_" + randStr(),
		Password: "$2a$10$dummyhashvaluethatisirrelevantfortest",
		Quota:    quota,
		Status:   model.UserStatusEnabled,
		Role:     model.RoleCommonUser,
	}
	if err := model.DB.Create(u).Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}
}

var randCounter int
var randMu sync.Mutex

func randStr() string {
	randMu.Lock()
	defer randMu.Unlock()
	randCounter++
	return string(rune('a'+(randCounter%26))) + string(rune('0'+(randCounter%10)))
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func callInternalTopup(body []byte) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := gin.New()
	r.POST("/api/internal/topup", InternalTopup)
	req := httptest.NewRequest("POST", "/api/internal/topup", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	return w
}

func decodeResp(t *testing.T, body []byte) (bool, string, map[string]any) {
	t.Helper()
	var resp struct {
		Success bool           `json:"success"`
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Data    map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode resp: %v (body=%s)", err, string(body))
	}
	return resp.Success, resp.Code, resp.Data
}

func getUserQuota(t *testing.T, userId int) int64 {
	t.Helper()
	q, err := model.GetUserQuota(userId)
	if err != nil {
		t.Fatalf("get quota: %v", err)
	}
	return q
}

func TestInternalTopup_HappyPath(t *testing.T) {
	setupTestDB(t)
	seedUser(t, 100, 0)

	body := mustJSON(t, InternalTopupRequest{
		Action: "topup", OrderNo: "IDR20240517TEST0001",
		UserId: 100, Quota: 1_000_000, Remark: "qris payment",
	})
	w := callInternalTopup(body)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	ok, _, data := decodeResp(t, w.Body.Bytes())
	if !ok {
		t.Fatalf("expected success=true, body=%s", w.Body.String())
	}
	if data["new_balance"].(float64) != 1_000_000 {
		t.Fatalf("expected new_balance=1000000, got %v", data["new_balance"])
	}
	if data["idempotent_replay"].(bool) {
		t.Fatalf("first call should not be a replay")
	}
	if getUserQuota(t, 100) != 1_000_000 {
		t.Fatalf("user quota mismatch")
	}
}

func TestInternalTopup_Idempotency(t *testing.T) {
	setupTestDB(t)
	seedUser(t, 101, 500)

	body := mustJSON(t, InternalTopupRequest{
		Action: "topup", OrderNo: "IDR20240517IDEMP01",
		UserId: 101, Quota: 1000,
	})

	w1 := callInternalTopup(body)
	if w1.Code != http.StatusOK {
		t.Fatalf("first call: %d %s", w1.Code, w1.Body.String())
	}

	w2 := callInternalTopup(body)
	if w2.Code != http.StatusOK {
		t.Fatalf("second call: %d %s", w2.Code, w2.Body.String())
	}
	_, _, data2 := decodeResp(t, w2.Body.Bytes())
	if !data2["idempotent_replay"].(bool) {
		t.Fatalf("second call should be a replay, got %v", data2)
	}
	if got := getUserQuota(t, 101); got != 1500 {
		t.Fatalf("balance must NOT be doubled. expected 1500, got %d", got)
	}
}

func TestInternalTopup_Refund_HappyPath(t *testing.T) {
	setupTestDB(t)
	seedUser(t, 102, 5000)

	body := mustJSON(t, InternalTopupRequest{
		Action: "refund", OrderNo: "IDR20240517REFUND1",
		UserId: 102, Quota: 1500, Remark: "full refund",
	})
	w := callInternalTopup(body)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	_, _, data := decodeResp(t, w.Body.Bytes())
	if data["new_balance"].(float64) != 3500 {
		t.Fatalf("expected new_balance=3500, got %v", data["new_balance"])
	}
	if data["quota"].(float64) != -1500 {
		t.Fatalf("expected signed quota=-1500, got %v", data["quota"])
	}
	if getUserQuota(t, 102) != 3500 {
		t.Fatalf("user quota mismatch")
	}
}

func TestInternalTopup_Refund_InsufficientBalance(t *testing.T) {
	setupTestDB(t)
	seedUser(t, 103, 100)

	body := mustJSON(t, InternalTopupRequest{
		Action: "refund", OrderNo: "IDR20240517INSUF01",
		UserId: 103, Quota: 1000,
	})
	w := callInternalTopup(body)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", w.Code, w.Body.String())
	}
	ok, code, _ := decodeResp(t, w.Body.Bytes())
	if ok {
		t.Fatalf("expected success=false")
	}
	if code != "insufficient_balance" {
		t.Fatalf("expected code=insufficient_balance, got %q", code)
	}
	// CRITICAL: balance must be unchanged after a failed refund.
	if got := getUserQuota(t, 103); got != 100 {
		t.Fatalf("balance must NOT change on insufficient_balance. expected 100, got %d", got)
	}
	// And the record must NOT have been created (otherwise a retry after
	// topping up would be incorrectly treated as a replay).
	if _, err := model.GetInternalTopupRecord("IDR20240517INSUF01", "refund"); !model.IsRecordNotFound(err) {
		t.Fatalf("record must not exist after rollback, got err=%v", err)
	}
}

func TestInternalTopup_Validation(t *testing.T) {
	setupTestDB(t)
	seedUser(t, 104, 0)

	cases := []struct {
		name string
		req  InternalTopupRequest
	}{
		{"empty action", InternalTopupRequest{OrderNo: "X", UserId: 104, Quota: 1}},
		{"bad action", InternalTopupRequest{Action: "foo", OrderNo: "X", UserId: 104, Quota: 1}},
		{"empty order_no", InternalTopupRequest{Action: "topup", UserId: 104, Quota: 1}},
		{"long order_no", InternalTopupRequest{Action: "topup", OrderNo: strings.Repeat("x", 65), UserId: 104, Quota: 1}},
		{"zero user_id", InternalTopupRequest{Action: "topup", OrderNo: "X", UserId: 0, Quota: 1}},
		{"negative user_id", InternalTopupRequest{Action: "topup", OrderNo: "X", UserId: -1, Quota: 1}},
		{"zero quota", InternalTopupRequest{Action: "topup", OrderNo: "X", UserId: 104, Quota: 0}},
		{"negative quota", InternalTopupRequest{Action: "topup", OrderNo: "X", UserId: 104, Quota: -1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := callInternalTopup(mustJSON(t, tc.req))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestInternalTopup_UserNotFound(t *testing.T) {
	setupTestDB(t)
	body := mustJSON(t, InternalTopupRequest{
		Action: "topup", OrderNo: "IDR20240517NOUSER1",
		UserId: 9999, Quota: 1000,
	})
	w := callInternalTopup(body)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
	_, code, _ := decodeResp(t, w.Body.Bytes())
	if code != "user_not_found" {
		t.Fatalf("expected code=user_not_found, got %q", code)
	}
}

func TestInternalTopup_DifferentActionsSameOrderNo(t *testing.T) {
	// A topup and a refund with the same order_no are different rows in the
	// idempotency table (composite key on order_no+action), so both should
	// process. This matches PR #2's refunds-after-topup flow.
	setupTestDB(t)
	seedUser(t, 105, 0)

	w1 := callInternalTopup(mustJSON(t, InternalTopupRequest{
		Action: "topup", OrderNo: "IDR20240517SAME001", UserId: 105, Quota: 2000,
	}))
	if w1.Code != http.StatusOK {
		t.Fatalf("topup: %d %s", w1.Code, w1.Body.String())
	}

	w2 := callInternalTopup(mustJSON(t, InternalTopupRequest{
		Action: "refund", OrderNo: "IDR20240517SAME001", UserId: 105, Quota: 500,
	}))
	if w2.Code != http.StatusOK {
		t.Fatalf("refund: %d %s", w2.Code, w2.Body.String())
	}

	if got := getUserQuota(t, 105); got != 1500 {
		t.Fatalf("expected 1500 (2000 topup - 500 refund), got %d", got)
	}
}

func TestIsDuplicateKeyError(t *testing.T) {
	cases := map[string]bool{
		"":                                              false,
		"some other error":                              false,
		"Error 1062: Duplicate entry 'foo' for key 'X'": true,
		"UNIQUE constraint failed: x.order_no":          true,
		"ERROR: duplicate key value violates unique":    true,
	}
	for msg, expect := range cases {
		var err error
		if msg != "" {
			err = errorWithMsg(msg)
		}
		got := isDuplicateKeyError(err)
		if got != expect {
			t.Errorf("isDuplicateKeyError(%q) = %v, want %v", msg, got, expect)
		}
	}
}

type fakeErr struct{ s string }

func (e *fakeErr) Error() string { return e.s }
func errorWithMsg(s string) error { return &fakeErr{s: s} }

// Compile-time placeholder so we don't accidentally drop the import:
var _ = os.Getenv
