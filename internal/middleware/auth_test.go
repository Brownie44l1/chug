package middleware

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Brownie44l1/chug/internal/config"
	"github.com/Brownie44l1/chug/internal/db"
	"github.com/Brownie44l1/chug/internal/redis"
	goredis "github.com/redis/go-redis/v9"
)

func TestAuthMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Load configuration
	cfg := config.Load()
	if cfg.PostgresURL == "" || cfg.RedisURL == "" {
		t.Skip("Skipping test: database or redis url not configured in environment")
	}

	// Connect to PostgreSQL and Redis
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
	`, "test-dev@example.com", "testpass").Scan(&devID)
	require.NoError(t, err)

	validKey := "chug_valid_key_123"
	hashedValidKey := hashKey(validKey)
	var validKeyID string

	revokedKey := "chug_revoked_key_456"
	hashedRevokedKey := hashKey(revokedKey)
	var revokedKeyID string

	// Clean existing if any to avoid test pollution
	_, _ = pgDB.Exec("DELETE FROM api_keys WHERE key_hash IN ($1, $2) OR developer_id = $3", hashedValidKey, hashedRevokedKey, devID)
	_ = rdb.Del(ctx(), "auth:key:"+hashedValidKey)
	_ = rdb.Del(ctx(), "auth:key:"+hashedRevokedKey)

	err = pgDB.QueryRow(`
		INSERT INTO api_keys (developer_id, key_hash, label, is_active)
		VALUES ($1, $2, $3, $4)
		RETURNING id
	`, devID, hashedValidKey, "test-valid", true).Scan(&validKeyID)
	require.NoError(t, err)

	err = pgDB.QueryRow(`
		INSERT INTO api_keys (developer_id, key_hash, label, is_active, revoked_at)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id
	`, devID, hashedRevokedKey, "test-revoked", false, time.Now()).Scan(&revokedKeyID)
	require.NoError(t, err)

	// Set up Gin Router
	r := gin.New()
	r.Use(Auth(hashSecret))
	r.GET("/test", func(c *gin.Context) {
		developerID, _ := c.Get("developer_id")
		apiKeyID, _ := c.Get("api_key_id")
		c.JSON(http.StatusOK, gin.H{
			"developer_id": developerID,
			"api_key_id":   apiKeyID,
		})
	})

	// 1. Missing Authorization header
	t.Run("Missing Authorization Header", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/test", nil)
		r.ServeHTTP(w, req)

		assert.Equal(t, http.StatusUnauthorized, w.Code)
		assert.Contains(t, w.Body.String(), "Missing Authorization header")
	})

	// 2. Invalid API Key
	t.Run("Invalid API Key", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/test", nil)
		req.Header.Set("Authorization", "Bearer invalidkey")
		r.ServeHTTP(w, req)

		assert.Equal(t, http.StatusUnauthorized, w.Code)
		assert.Contains(t, w.Body.String(), "Invalid API key")
	})

	// 3. Revoked API Key
	t.Run("Revoked API Key", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/test", nil)
		req.Header.Set("Authorization", "Bearer "+revokedKey)
		r.ServeHTTP(w, req)

		assert.Equal(t, http.StatusUnauthorized, w.Code)
		assert.Contains(t, w.Body.String(), "Revoked API key")
	})

	// 4. Valid API Key
	t.Run("Valid API Key", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/test", nil)
		req.Header.Set("Authorization", "Bearer "+validKey)
		r.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)
		assert.JSONEq(t, fmt.Sprintf(`{"developer_id":"%s","api_key_id":"%s"}`, devID, validKeyID), w.Body.String())
	})

	// 5. Redis Caching verified
	t.Run("Cache Hit on subsequent requests", func(t *testing.T) {
		cacheKey := "auth:key:" + hashedValidKey
		err := rdb.Del(ctx(), cacheKey).Err()
		require.NoError(t, err)

		// First request (Cache Miss, hits DB, populates Redis)
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/test", nil)
		req.Header.Set("Authorization", "Bearer "+validKey)
		r.ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code)

		// Check that cache is now populated in Redis
		ttl, err := rdb.TTL(ctx(), cacheKey).Result()
		require.NoError(t, err)
		assert.True(t, ttl > 0 && ttl <= 5*time.Minute)

		// Now temporarily break the database connection to prove it does not hit DB
		originalDB := db.DB
		db.DB = nil
		defer func() { db.DB = originalDB }()

		// Second request should hit cache and succeed without Postgres
		w2 := httptest.NewRecorder()
		req2, _ := http.NewRequest("GET", "/test", nil)
		req2.Header.Set("Authorization", "Bearer "+validKey)
		r.ServeHTTP(w2, req2)
		assert.Equal(t, http.StatusOK, w2.Code)
		assert.JSONEq(t, fmt.Sprintf(`{"developer_id":"%s","api_key_id":"%s"}`, devID, validKeyID), w2.Body.String())
	})

	// 6. Redis Unavailable returns 503
	t.Run("Redis Down Returns 503 Service Unavailable", func(t *testing.T) {
		originalClient := redis.Client
		// Replace redis client with a dead/invalid one
		redis.Client = goredis.NewClient(&goredis.Options{
			Addr:        "localhost:9999", // Broken address
			DialTimeout: 10 * time.Millisecond,
		})
		defer func() { redis.Client = originalClient }()

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/test", nil)
		req.Header.Set("Authorization", "Bearer "+validKey)
		r.ServeHTTP(w, req)

		// Should fail closed with 503 Service Unavailable
		assert.Equal(t, http.StatusServiceUnavailable, w.Code)
		assert.Contains(t, w.Body.String(), "Service temporarily unavailable")
	})
}

func ctx() context.Context {
	return context.Background()
}
