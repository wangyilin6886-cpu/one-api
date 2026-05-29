// Package errors carries the canonical error code list for the payment
// service. Every error returned to a user is registered here. The format is:
//
//	{ "code": "ORDER_AMOUNT_TOO_LOW",
//	  "message":    "<localized>",
//	  "message_en": "Top-up amount is below the minimum.",
//	  "message_zh": "充值金额低于最低限额。",
//	  "message_id": "Jumlah top-up di bawah batas minimum." }
//
// `code` is a stable machine identifier (never translated).
// The HTTP handler picks the language from Accept-Language; supported:
//   - zh / zh-* -> Chinese
//   - id / id-* -> Indonesian
//   - everything else -> English (default for a global audience)
package errors

import (
	"net/http"
	"strings"
)

// Code is the stable identifier returned in the `code` JSON field.
type Code string

const (
	// 400 - invalid input
	InvalidRequestBody Code = "INVALID_REQUEST_BODY"
	InvalidParameter   Code = "INVALID_PARAMETER"
	OrderAmountTooLow  Code = "ORDER_AMOUNT_TOO_LOW"
	OrderAmountTooHigh Code = "ORDER_AMOUNT_TOO_HIGH"
	UnsupportedCurrency Code = "UNSUPPORTED_CURRENCY"

	// 401 - auth
	Unauthorized        Code = "UNAUTHORIZED"
	WebhookBadSignature Code = "WEBHOOK_BAD_SIGNATURE"

	// 403 - forbidden
	OrderForbidden Code = "ORDER_FORBIDDEN"

	// 404 - not found
	OrderNotFound Code = "ORDER_NOT_FOUND"
	UserNotFound  Code = "USER_NOT_FOUND"

	// 409 - conflict
	OrderStateConflict Code = "ORDER_STATE_CONFLICT"
	DuplicateWebhook   Code = "DUPLICATE_WEBHOOK"

	// 500 / 502 - server / upstream
	ProviderCallFailed Code = "PROVIDER_CALL_FAILED"
	OneAPICallFailed   Code = "ONEAPI_CALL_FAILED"
	DatabaseError      Code = "DATABASE_ERROR"
	InternalError      Code = "INTERNAL_ERROR"
	ConfigError        Code = "CONFIG_ERROR"

	// 503 - service unavailable
	ServiceUnavailable Code = "SERVICE_UNAVAILABLE"
)

// LocalizedMessage holds the trilingual strings for one code.
type LocalizedMessage struct {
	En string
	Zh string
	Id string
}

var messages = map[Code]LocalizedMessage{
	InvalidRequestBody:  {En: "Invalid request body.", Zh: "请求体格式无效。", Id: "Permintaan tidak valid."},
	InvalidParameter:    {En: "Invalid parameter.", Zh: "参数无效。", Id: "Parameter tidak valid."},
	OrderAmountTooLow:   {En: "Top-up amount is below the minimum.", Zh: "充值金额低于最低限额。", Id: "Jumlah top-up di bawah batas minimum."},
	OrderAmountTooHigh:  {En: "Top-up amount exceeds the maximum.", Zh: "充值金额超出最高限额。", Id: "Jumlah top-up melebihi batas maksimum."},
	UnsupportedCurrency: {En: "Currency is not supported.", Zh: "不支持的货币。", Id: "Mata uang tidak didukung."},

	Unauthorized:        {En: "Authentication required.", Zh: "需要身份验证。", Id: "Autentikasi diperlukan."},
	WebhookBadSignature: {En: "Webhook signature is invalid.", Zh: "Webhook 签名无效。", Id: "Tanda tangan webhook tidak valid."},

	OrderForbidden: {En: "You are not allowed to access this order.", Zh: "无权访问该订单。", Id: "Anda tidak boleh mengakses pesanan ini."},

	OrderNotFound: {En: "Order not found.", Zh: "订单不存在。", Id: "Pesanan tidak ditemukan."},
	UserNotFound:  {En: "User not found.", Zh: "用户不存在。", Id: "Pengguna tidak ditemukan."},

	OrderStateConflict: {En: "Order is not in a state that allows this action.", Zh: "订单当前状态不允许该操作。", Id: "Status pesanan tidak mengizinkan operasi ini."},
	DuplicateWebhook:   {En: "Webhook already processed.", Zh: "Webhook 已处理过。", Id: "Webhook sudah diproses."},

	ProviderCallFailed: {En: "Payment provider call failed.", Zh: "支付服务商调用失败。", Id: "Gagal memanggil penyedia pembayaran."},
	OneAPICallFailed:   {En: "Failed to call one-api.", Zh: "调用 one-api 失败。", Id: "Gagal memanggil one-api."},
	DatabaseError:      {En: "Database error.", Zh: "数据库错误。", Id: "Kesalahan basis data."},
	InternalError:      {En: "Internal server error.", Zh: "服务器内部错误。", Id: "Kesalahan server internal."},
	ConfigError:        {En: "Server is misconfigured.", Zh: "服务端配置错误。", Id: "Konfigurasi server bermasalah."},

	ServiceUnavailable: {En: "Service temporarily unavailable.", Zh: "服务暂时不可用。", Id: "Layanan sementara tidak tersedia."},
}

// Default HTTP status code for each Code. Handlers may override per call
// (e.g. distinguish "duplicate" between 200 and 409 depending on flow).
var defaultStatus = map[Code]int{
	InvalidRequestBody:  http.StatusBadRequest,
	InvalidParameter:    http.StatusBadRequest,
	OrderAmountTooLow:   http.StatusBadRequest,
	OrderAmountTooHigh:  http.StatusBadRequest,
	UnsupportedCurrency: http.StatusBadRequest,

	Unauthorized:        http.StatusUnauthorized,
	WebhookBadSignature: http.StatusUnauthorized,

	OrderForbidden: http.StatusForbidden,

	OrderNotFound: http.StatusNotFound,
	UserNotFound:  http.StatusNotFound,

	OrderStateConflict: http.StatusConflict,
	DuplicateWebhook:   http.StatusOK, // an idempotent replay is a successful no-op

	ProviderCallFailed: http.StatusBadGateway,
	OneAPICallFailed:   http.StatusBadGateway,
	DatabaseError:      http.StatusInternalServerError,
	InternalError:      http.StatusInternalServerError,
	ConfigError:        http.StatusInternalServerError,

	ServiceUnavailable: http.StatusServiceUnavailable,
}

// HTTPStatus returns the default HTTP status code for the given Code.
func (c Code) HTTPStatus() int {
	if s, ok := defaultStatus[c]; ok {
		return s
	}
	return http.StatusInternalServerError
}

// Message returns (en, zh, id) for the given Code.
func (c Code) Message() (string, string, string) {
	if m, ok := messages[c]; ok {
		return m.En, m.Zh, m.Id
	}
	return string(c), string(c), string(c)
}

// PickLang inspects Accept-Language and returns "zh" / "id" / "en".
// Picky parsing is unnecessary - we only support three options.
func PickLang(acceptLanguage string) string {
	s := strings.ToLower(strings.TrimSpace(acceptLanguage))
	switch {
	case strings.HasPrefix(s, "zh"), strings.Contains(s, ",zh"), strings.Contains(s, " zh"):
		return "zh"
	case strings.HasPrefix(s, "id"), strings.Contains(s, ",id"), strings.Contains(s, " id"):
		return "id"
	}
	return "en"
}

// Response builds the JSON body returned alongside an error. All three
// languages are always included so the frontend / debugger has parity
// regardless of which language is currently selected. `message` carries
// the language picked by Accept-Language.
type Response struct {
	Success   bool   `json:"success"`
	Code      Code   `json:"code"`
	Message   string `json:"message"`
	MessageEn string `json:"message_en"`
	MessageZh string `json:"message_zh"`
	MessageId string `json:"message_id"`
	Detail    string `json:"detail,omitempty"`
}

// Build returns a Response struct localized for `lang` ("en"/"zh"/"id").
// `detail` is an optional free-form string for additional context (shown
// only in non-prod or when explicitly enabled).
func Build(code Code, lang, detail string) Response {
	en, zh, id := code.Message()
	main := en
	switch lang {
	case "zh":
		main = zh
	case "id":
		main = id
	}
	return Response{
		Success:   false,
		Code:      code,
		Message:   main,
		MessageEn: en,
		MessageZh: zh,
		MessageId: id,
		Detail:    detail,
	}
}
