// Package errors carries the canonical error code list for the payment
// service. Every error returned to a user lives here. The format is:
//
//	{ "code": "ORDER_AMOUNT_TOO_LOW",
//	  "message_en": "Top-up amount is below the minimum.",
//	  "message_id": "Jumlah top-up di bawah batas minimum." }
//
// `code` is a stable machine identifier. `message_en` / `message_id` are the
// strings the front-end displays. The HTTP handler picks one based on
// Accept-Language: "id"/"id-ID" → Indonesian, anything else → English.
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
	UnsupportedMethod  Code = "UNSUPPORTED_PAYMENT_METHOD"

	// 401 - auth
	Unauthorized    Code = "UNAUTHORIZED"
	WebhookBadToken Code = "WEBHOOK_BAD_TOKEN"
	IPNotAllowed    Code = "IP_NOT_ALLOWED"

	// 403 - forbidden
	OrderForbidden Code = "ORDER_FORBIDDEN"

	// 404 - not found
	OrderNotFound Code = "ORDER_NOT_FOUND"
	UserNotFound  Code = "USER_NOT_FOUND"

	// 409 - conflict
	OrderStateConflict Code = "ORDER_STATE_CONFLICT"
	DuplicateWebhook   Code = "DUPLICATE_WEBHOOK"

	// 500 - server
	XenditCallFailed   Code = "XENDIT_CALL_FAILED"
	OneAPICallFailed   Code = "ONEAPI_CALL_FAILED"
	DatabaseError      Code = "DATABASE_ERROR"
	InternalError      Code = "INTERNAL_ERROR"
	ConfigError        Code = "CONFIG_ERROR"

	// 503 - service unavailable
	ServiceUnavailable Code = "SERVICE_UNAVAILABLE"
)

// LocalizedMessage holds the bilingual strings for one code.
type LocalizedMessage struct {
	En string
	Id string
}

var messages = map[Code]LocalizedMessage{
	InvalidRequestBody: {En: "Invalid request body.", Id: "Permintaan tidak valid."},
	InvalidParameter:   {En: "Invalid parameter.", Id: "Parameter tidak valid."},
	OrderAmountTooLow:  {En: "Top-up amount is below the minimum.", Id: "Jumlah top-up di bawah batas minimum."},
	OrderAmountTooHigh: {En: "Top-up amount exceeds the maximum.", Id: "Jumlah top-up melebihi batas maksimum."},
	UnsupportedMethod:  {En: "Payment method is not supported.", Id: "Metode pembayaran tidak didukung."},

	Unauthorized:    {En: "Authentication required.", Id: "Autentikasi diperlukan."},
	WebhookBadToken: {En: "Webhook token is invalid.", Id: "Token webhook tidak valid."},
	IPNotAllowed:    {En: "Source IP is not allowed.", Id: "IP sumber tidak diizinkan."},

	OrderForbidden: {En: "You are not allowed to access this order.", Id: "Anda tidak boleh mengakses pesanan ini."},

	OrderNotFound: {En: "Order not found.", Id: "Pesanan tidak ditemukan."},
	UserNotFound:  {En: "User not found.", Id: "Pengguna tidak ditemukan."},

	OrderStateConflict: {En: "Order is not in a state that allows this action.", Id: "Status pesanan tidak mengizinkan operasi ini."},
	DuplicateWebhook:   {En: "Webhook already processed.", Id: "Webhook sudah diproses."},

	XenditCallFailed: {En: "Failed to call Xendit.", Id: "Gagal memanggil Xendit."},
	OneAPICallFailed: {En: "Failed to call one-api.", Id: "Gagal memanggil one-api."},
	DatabaseError:    {En: "Database error.", Id: "Kesalahan basis data."},
	InternalError:    {En: "Internal server error.", Id: "Kesalahan server internal."},
	ConfigError:      {En: "Server is misconfigured.", Id: "Konfigurasi server bermasalah."},

	ServiceUnavailable: {En: "Service temporarily unavailable.", Id: "Layanan sementara tidak tersedia."},
}

// Default HTTP status code for each Code. Handlers may override on a per-call
// basis (e.g. distinguish "duplicate" between 200 and 409 depending on flow).
var defaultStatus = map[Code]int{
	InvalidRequestBody: http.StatusBadRequest,
	InvalidParameter:   http.StatusBadRequest,
	OrderAmountTooLow:  http.StatusBadRequest,
	OrderAmountTooHigh: http.StatusBadRequest,
	UnsupportedMethod:  http.StatusBadRequest,

	Unauthorized:    http.StatusUnauthorized,
	WebhookBadToken: http.StatusUnauthorized,
	IPNotAllowed:    http.StatusForbidden,

	OrderForbidden: http.StatusForbidden,

	OrderNotFound: http.StatusNotFound,
	UserNotFound:  http.StatusNotFound,

	OrderStateConflict: http.StatusConflict,
	DuplicateWebhook:   http.StatusOK, // a duplicate is a successful idempotent replay

	XenditCallFailed: http.StatusBadGateway,
	OneAPICallFailed: http.StatusBadGateway,
	DatabaseError:    http.StatusInternalServerError,
	InternalError:    http.StatusInternalServerError,
	ConfigError:      http.StatusInternalServerError,

	ServiceUnavailable: http.StatusServiceUnavailable,
}

// HTTPStatus returns the default HTTP status code for the given Code.
func (c Code) HTTPStatus() int {
	if s, ok := defaultStatus[c]; ok {
		return s
	}
	return http.StatusInternalServerError
}

// Message returns (en, id) for the given Code.
func (c Code) Message() (string, string) {
	if m, ok := messages[c]; ok {
		return m.En, m.Id
	}
	return string(c), string(c)
}

// PickLang inspects Accept-Language and returns "id" for Indonesian-preferring
// callers, otherwise "en". Picky parsing is unnecessary here — we only need to
// distinguish two languages.
func PickLang(acceptLanguage string) string {
	s := strings.ToLower(strings.TrimSpace(acceptLanguage))
	if strings.HasPrefix(s, "id") || strings.Contains(s, ",id;") || strings.Contains(s, " id;") {
		return "id"
	}
	return "en"
}

// Response builds the JSON body returned alongside an error. Both messages
// are always included so debugging tools have both languages available; the
// "message" field uses the language picked by Accept-Language.
type Response struct {
	Success   bool   `json:"success"`
	Code      Code   `json:"code"`
	Message   string `json:"message"`
	MessageEn string `json:"message_en"`
	MessageId string `json:"message_id"`
	Detail    string `json:"detail,omitempty"`
}

// Build returns a Response struct localized for `lang` ("id" or "en").
// `detail` is an optional free-form string for additional context shown only
// in non-prod or when explicitly enabled.
func Build(code Code, lang, detail string) Response {
	en, id := code.Message()
	main := en
	if lang == "id" {
		main = id
	}
	return Response{
		Success:   false,
		Code:      code,
		Message:   main,
		MessageEn: en,
		MessageId: id,
		Detail:    detail,
	}
}
