package middleware

import (
	"github.com/gin-gonic/gin"

	apierrors "github.com/songquanpeng/one-api/payment/internal/errors"
)

// respondError aborts the request with a localized error body. Shared by
// all middlewares in this package so each one doesn't roll its own
// inconsistent JSON shape.
func respondError(c *gin.Context, code apierrors.Code, detail string) {
	lang := apierrors.PickLang(c.Request.Header.Get("Accept-Language"))
	c.AbortWithStatusJSON(code.HTTPStatus(), apierrors.Build(code, lang, detail))
}
