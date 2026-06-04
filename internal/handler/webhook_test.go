package handler

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Brownie44l1/chug/internal/config"
	"github.com/Brownie44l1/chug/internal/db"
	"github.com/Brownie44l1/chug/internal/middleware"
	"github.com/Brownie44l1/chug/internal/redis"
)

func TestWebhookHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cfg := config.Load()
	if cfg.PostgresURL == "" || cfg.RedisURL == "" {
		t.Skip("Skipping webhook handler tests: database or redis url not configured")
	}

	pgDB, err := db.Connect(cfg.PostgresURL)
	require.NoError(t, err)
	defer pgDB.Close()

	rdb, err := redis.Connect(cfg.RedisURL)
	require.NoError(t, err)
	defer rdb.Close()

	hashSecret := "test-secret-key"

	// Helper to hash key
	hashKey := func(key string) string {
		h := hmac.New(sha256.New, []byte(hashSecret))
		h.Write([]byte(key))
		return hex.EncodeToString(h.Sum(nil))
	}

	// Seed test developer and keys
	var devID string
	err = pgDB.QueryRow(`
		INSERT INTO developers (email, hashed_password)
		VALUES ($1, $2)
		ON CONFLICT (email) DO UPDATE SET email = EXCLUDED.email
		RETURNING id
	`, "test-webhook-dev@example.com", "testpass").Scan(&devID)
	require.NoError(t, err)

	validKey := "chug_webhook_valid_key"
	hashedValidKey := hashKey(validKey)
	var validKeyID string

	// Clean database state
	_, _ = pgDB.Exec("DELETE FROM webhook_endpoints WHERE developer_id = $1", devID)
	_, _ = pgDB.Exec("DELETE FROM api_keys WHERE key_hash = $1 OR developer_id = $2", hashedValidKey, devID)
	_ = rdb.Del(context.Background(), "auth:key:"+hashedValidKey)

	err = pgDB.QueryRow(`
		INSERT INTO api_keys (developer_id, key_hash, label, is_active)
		VALUES ($1, $2, $3, $4)
		RETURNING id
	`, devID, hashedValidKey, "test-webhook-valid", true).Scan(&validKeyID)
	require.NoError(t, err)

	// Setup Router
	r := gin.New()
	webhookHandler := NewWebhookHandler(pgDB)

	r.POST("/webhooks", middleware.Auth(hashSecret), webhookHandler.Register)
	r.GET("/webhooks", middleware.Auth(hashSecret), webhookHandler.List)
	r.DELETE("/webhooks/:id", middleware.Auth(hashSecret), webhookHandler.Delete)

	t.Run("Unauthorized Request", func(t *testing.T) {
		req, err := http.NewRequest("POST", "/webhooks", bytes.NewBufferString(`{"url":"https://example.com/callback"}`))
		require.NoError(t, err)

		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("Register Webhook - Invalid URL", func(t *testing.T) {
		// HTTP is not allowed (only HTTPS)
		payload := `{"url":"http://example.com/callback"}`
		req, err := http.NewRequest("POST", "/webhooks", bytes.NewBufferString(payload))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+validKey)
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Contains(t, w.Body.String(), "must be a valid HTTPS address")
	})

	t.Run("Register Webhook - Missing URL", func(t *testing.T) {
		payload := `{"url":""}`
		req, err := http.NewRequest("POST", "/webhooks", bytes.NewBufferString(payload))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+validKey)
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	var createdWebhookID string
	var createdSecret string

	t.Run("Register Webhook - Success", func(t *testing.T) {
		payload := `{"url":"https://example.com/callback"}`
		req, err := http.NewRequest("POST", "/webhooks", bytes.NewBufferString(payload))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+validKey)
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		assert.Equal(t, http.StatusCreated, w.Code)

		var resp map[string]interface{}
		err = json.Unmarshal(w.Body.Bytes(), &resp)
		require.NoError(t, err)

		assert.NotEmpty(t, resp["id"])
		assert.Equal(t, devID, resp["developer_id"])
		assert.Equal(t, "https://example.com/callback", resp["url"])
		assert.Equal(t, true, resp["is_active"])
		assert.NotEmpty(t, resp["secret"])

		createdWebhookID = resp["id"].(string)
		createdSecret = resp["secret"].(string)

		// Verify database entry
		var dbSecret string
		var dbURL string
		var dbIsActive bool
		err = pgDB.QueryRow(`
			SELECT url, secret, is_active
			FROM webhook_endpoints
			WHERE id = $1
		`, createdWebhookID).Scan(&dbURL, &dbSecret, &dbIsActive)
		require.NoError(t, err)

		assert.Equal(t, "https://example.com/callback", dbURL)
		assert.Equal(t, createdSecret, dbSecret)
		assert.True(t, dbIsActive)
	})

	t.Run("List Webhooks - Success", func(t *testing.T) {
		req, err := http.NewRequest("GET", "/webhooks", nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+validKey)

		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)

		var list []map[string]interface{}
		err = json.Unmarshal(w.Body.Bytes(), &list)
		require.NoError(t, err)

		assert.Len(t, list, 1)
		assert.Equal(t, createdWebhookID, list[0]["id"])
		assert.Equal(t, "https://example.com/callback", list[0]["url"])
		// Verify secret is NOT included in the list response
		assert.Nil(t, list[0]["secret"])
	})

	t.Run("Delete Webhook - Forbidden (Other Developer)", func(t *testing.T) {
		// Seed another developer
		var otherDevID string
		err = pgDB.QueryRow(`
			INSERT INTO developers (email, hashed_password)
			VALUES ($1, $2)
			ON CONFLICT (email) DO UPDATE SET email = EXCLUDED.email
			RETURNING id
		`, "other-webhook-dev@example.com", "testpass").Scan(&otherDevID)
		require.NoError(t, err)

		var otherWebhookID string
		err = pgDB.QueryRow(`
			INSERT INTO webhook_endpoints (developer_id, url, secret)
			VALUES ($1, $2, $3)
			RETURNING id
		`, otherDevID, "https://other.com/cb", "secret123").Scan(&otherWebhookID)
		require.NoError(t, err)

		defer func() {
			_, _ = pgDB.Exec("DELETE FROM webhook_endpoints WHERE id = $1", otherWebhookID)
		}()

		req, err := http.NewRequest("DELETE", "/webhooks/"+otherWebhookID, nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+validKey)

		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		assert.Equal(t, http.StatusForbidden, w.Code)
	})

	t.Run("Delete Webhook - Not Found", func(t *testing.T) {
		req, err := http.NewRequest("DELETE", "/webhooks/00000000-0000-0000-0000-000000000000", nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+validKey)

		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		assert.Equal(t, http.StatusNotFound, w.Code)
	})

	t.Run("Delete Webhook - Success", func(t *testing.T) {
		req, err := http.NewRequest("DELETE", "/webhooks/"+createdWebhookID, nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+validKey)

		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)

		// Verify database soft delete (is_active is false)
		var dbIsActive bool
		err = pgDB.QueryRow(`
			SELECT is_active
			FROM webhook_endpoints
			WHERE id = $1
		`, createdWebhookID).Scan(&dbIsActive)
		require.NoError(t, err)
		assert.False(t, dbIsActive)

		// Verify that it is no longer returned in List endpoint
		reqList, err := http.NewRequest("GET", "/webhooks", nil)
		require.NoError(t, err)
		reqList.Header.Set("Authorization", "Bearer "+validKey)

		wList := httptest.NewRecorder()
		r.ServeHTTP(wList, reqList)

		assert.Equal(t, http.StatusOK, wList.Code)
		var list []map[string]interface{}
		err = json.Unmarshal(wList.Body.Bytes(), &list)
		require.NoError(t, err)
		assert.Len(t, list, 0)
	})
}
