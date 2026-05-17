package controller

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/helper"
	"github.com/songquanpeng/one-api/common/logger"
	"github.com/songquanpeng/one-api/model"
)

// InternalTopupRequest is the body accepted by POST /api/internal/topup.
//
// The endpoint is shared by two actions:
//   - "topup":  add `quota` to the user's balance (used after a successful payment)
//   - "refund": subtract `quota` from the user's balance (used during a refund;
//     requires sufficient balance and runs under SELECT FOR UPDATE)
//
// `order_no` is the global idempotency key. The same (order_no, action) pair
// is processed at most once; replays return the original result.
type InternalTopupRequest struct {
	Action  string `json:"action"`
	OrderNo string `json:"order_no"`
	UserId  int    `json:"user_id"`
	Quota   int64  `json:"quota"`
	Remark  string `json:"remark"`
}

type InternalTopupResponseData struct {
	OrderNo          string `json:"order_no"`
	Action           string `json:"action"`
	UserId           int    `json:"user_id"`
	Quota            int64  `json:"quota"`
	NewBalance       int64  `json:"new_balance"`
	IdempotentReplay bool   `json:"idempotent_replay"`
}

var (
	errInsufficientBalance = errors.New("insufficient_balance")
	errDuplicateRecord     = errors.New("duplicate_record")
)

// InternalTopup handles POST /api/internal/topup. The route MUST be protected
// by middleware.InternalAuth so that only the payment-service can call it.
//
// Crediting and refunding both rely on a SELECT FOR UPDATE inside a DB
// transaction (matches the pattern in model.Redeem) to serialize concurrent
// changes to the user's balance. The internal_topup_records table provides
// idempotency: replay of the same (order_no, action) returns the previously
// computed balance without touching the user row.
func InternalTopup(c *gin.Context) {
	ctx := c.Request.Context()

	var req InternalTopupRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondInternalTopupError(c, http.StatusBadRequest, "invalid_request_body", err.Error())
		return
	}
	if err := validateInternalTopupRequest(&req); err != nil {
		respondInternalTopupError(c, http.StatusBadRequest, "invalid_parameter", err.Error())
		return
	}

	if existing, err := model.GetInternalTopupRecord(req.OrderNo, req.Action); err == nil && existing != nil {
		logger.Infof(ctx,
			"internal_topup idempotent replay: order_no=%s action=%s user_id=%d quota=%d new_balance=%d",
			existing.OrderNo, existing.Action, existing.UserId, existing.Quota, existing.NewBalance)
		respondInternalTopupSuccess(c, &InternalTopupResponseData{
			OrderNo:          existing.OrderNo,
			Action:           existing.Action,
			UserId:           existing.UserId,
			Quota:            existing.Quota,
			NewBalance:       existing.NewBalance,
			IdempotentReplay: true,
		})
		return
	} else if err != nil && !model.IsRecordNotFound(err) {
		respondInternalTopupError(c, http.StatusInternalServerError, "lookup_failed", err.Error())
		return
	}

	signedQuota := req.Quota
	if req.Action == model.InternalTopupActionRefund {
		signedQuota = -req.Quota
	}

	var newBalance int64
	txErr := model.DB.Transaction(func(tx *gorm.DB) error {
		var user model.User
		err := tx.Set("gorm:query_option", "FOR UPDATE").
			Where("id = ?", req.UserId).
			First(&user).Error
		if err != nil {
			return fmt.Errorf("lock user row: %w", err)
		}

		switch req.Action {
		case model.InternalTopupActionTopup:
			newBalance = user.Quota + req.Quota
		case model.InternalTopupActionRefund:
			if user.Quota < req.Quota {
				return errInsufficientBalance
			}
			newBalance = user.Quota - req.Quota
		default:
			return fmt.Errorf("unknown action: %s", req.Action)
		}

		record := &model.InternalTopupRecord{
			OrderNo:    req.OrderNo,
			Action:     req.Action,
			UserId:     req.UserId,
			Quota:      signedQuota,
			NewBalance: newBalance,
			Remark:     req.Remark,
			CreatedAt:  helper.GetTimestamp(),
		}
		if err := tx.Create(record).Error; err != nil {
			if isDuplicateKeyError(err) {
				return errDuplicateRecord
			}
			return fmt.Errorf("insert record: %w", err)
		}

		// Apply the delta atomically with the lock held. We use the gorm.Expr
		// form (`quota + ?` / `quota - ?`) for parity with the Redeem path,
		// even though `quota = newBalance` would be equivalent under the lock.
		var updateErr error
		if req.Action == model.InternalTopupActionTopup {
			updateErr = tx.Model(&model.User{}).
				Where("id = ?", req.UserId).
				Update("quota", gorm.Expr("quota + ?", req.Quota)).Error
		} else {
			updateErr = tx.Model(&model.User{}).
				Where("id = ?", req.UserId).
				Update("quota", gorm.Expr("quota - ?", req.Quota)).Error
		}
		if updateErr != nil {
			return fmt.Errorf("update user quota: %w", updateErr)
		}
		return nil
	})

	if errors.Is(txErr, errDuplicateRecord) {
		// Lost a tight race with a concurrent caller. The other side won —
		// fetch and return its result.
		existing, err := model.GetInternalTopupRecord(req.OrderNo, req.Action)
		if err != nil || existing == nil {
			respondInternalTopupError(c, http.StatusInternalServerError, "lookup_failed",
				"duplicate detected but record not found")
			return
		}
		logger.Infof(ctx,
			"internal_topup idempotent replay (race): order_no=%s action=%s user_id=%d",
			existing.OrderNo, existing.Action, existing.UserId)
		respondInternalTopupSuccess(c, &InternalTopupResponseData{
			OrderNo:          existing.OrderNo,
			Action:           existing.Action,
			UserId:           existing.UserId,
			Quota:            existing.Quota,
			NewBalance:       existing.NewBalance,
			IdempotentReplay: true,
		})
		return
	}
	if errors.Is(txErr, errInsufficientBalance) {
		logger.Errorf(ctx,
			"internal_topup refund rejected: order_no=%s user_id=%d quota=%d (insufficient balance)",
			req.OrderNo, req.UserId, req.Quota)
		respondInternalTopupError(c, http.StatusConflict, "insufficient_balance",
			"user balance is less than the requested refund amount")
		return
	}
	if txErr != nil {
		if errors.Is(txErr, gorm.ErrRecordNotFound) {
			respondInternalTopupError(c, http.StatusNotFound, "user_not_found",
				fmt.Sprintf("user_id=%d does not exist", req.UserId))
			return
		}
		respondInternalTopupError(c, http.StatusInternalServerError, "transaction_failed", txErr.Error())
		return
	}

	logQuota := int(req.Quota)
	if req.Action == model.InternalTopupActionRefund {
		logQuota = -logQuota
	}
	logContent := buildTopupLogContent(req.Action, req.OrderNo, req.Quota, req.Remark)
	model.RecordTopupLog(ctx, req.UserId, logContent, logQuota)

	// Refresh the cached quota so subsequent /v1 calls see the new balance
	// without waiting for the cache to expire.
	if err := model.CacheUpdateUserQuota(ctx, req.UserId); err != nil {
		logger.Errorf(ctx,
			"internal_topup: refresh user quota cache failed for user_id=%d: %s",
			req.UserId, err.Error())
	}

	logger.Infof(ctx,
		"internal_topup success: order_no=%s action=%s user_id=%d quota=%d new_balance=%d remark=%q",
		req.OrderNo, req.Action, req.UserId, req.Quota, newBalance, req.Remark)

	respondInternalTopupSuccess(c, &InternalTopupResponseData{
		OrderNo:          req.OrderNo,
		Action:           req.Action,
		UserId:           req.UserId,
		Quota:            signedQuota,
		NewBalance:       newBalance,
		IdempotentReplay: false,
	})
}

func validateInternalTopupRequest(req *InternalTopupRequest) error {
	req.Action = strings.TrimSpace(req.Action)
	req.OrderNo = strings.TrimSpace(req.OrderNo)
	req.Remark = strings.TrimSpace(req.Remark)

	if req.Action != model.InternalTopupActionTopup && req.Action != model.InternalTopupActionRefund {
		return fmt.Errorf("action must be %q or %q",
			model.InternalTopupActionTopup, model.InternalTopupActionRefund)
	}
	if req.OrderNo == "" {
		return errors.New("order_no is required")
	}
	if len(req.OrderNo) > 64 {
		return errors.New("order_no exceeds 64 chars")
	}
	if req.UserId <= 0 {
		return errors.New("user_id must be a positive integer")
	}
	if req.Quota <= 0 {
		return errors.New("quota must be a positive integer")
	}
	if len(req.Remark) > 255 {
		return errors.New("remark exceeds 255 chars")
	}
	return nil
}

func buildTopupLogContent(action, orderNo string, quota int64, remark string) string {
	var verb string
	if action == model.InternalTopupActionRefund {
		verb = "退款"
	} else {
		verb = "充值"
	}
	content := fmt.Sprintf("[%s] %s %s, 订单号 %s",
		strings.ToUpper(action), verb, common.LogQuota(quota), orderNo)
	if remark != "" {
		content += " (" + remark + ")"
	}
	return content
}

func respondInternalTopupSuccess(c *gin.Context, data *InternalTopupResponseData) {
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    data,
	})
}

func respondInternalTopupError(c *gin.Context, status int, code, message string) {
	c.JSON(status, gin.H{
		"success": false,
		"code":    code,
		"message": message,
	})
}

// isDuplicateKeyError detects unique-constraint violations across the three
// drivers one-api supports (MySQL, PostgreSQL, SQLite). GORM 1.25 does not
// translate driver errors by default, so we fall back to string matching.
func isDuplicateKeyError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "duplicate entry"): // MySQL
		return true
	case strings.Contains(msg, "unique constraint"): // PostgreSQL / SQLite
		return true
	case strings.Contains(msg, "duplicate key value"): // PostgreSQL alt wording
		return true
	}
	return false
}
