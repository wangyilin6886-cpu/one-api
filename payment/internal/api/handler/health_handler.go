package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// Health returns 200 with build info. Used by docker-compose healthcheck.
func Health(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Verify the DB is reachable - cheap ping query.
		sqlDB, err := db.DB()
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "db": err.Error()})
			return
		}
		if err := sqlDB.PingContext(c.Request.Context()); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "db": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "service": "payment", "version": "0.1.0"})
	}
}
