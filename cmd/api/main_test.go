package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Brownie44l1/chug/internal/config"
)

func TestHealthEndpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cfg := config.Load()
	r := gin.New()
	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{
			"status": "ok",
			"env":    cfg.Env,
		})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/health", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, 200, w.Code)
	assert.JSONEq(t, `{"status":"ok","env":"development"}`, w.Body.String())
}
