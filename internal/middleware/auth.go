package middleware

import (
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	goredis "github.com/redis/go-redis/v9"

	"github.com/Brownie44l1/chug/internal/db"
	chugredis "github.com/Brownie44l1/chug/internal/redis"
)

type CachedKeyInfo struct {
	APIKeyID    string `json:"api_key_id"`
	DeveloperID string `json:"developer_id"`
	IsActive    bool   `json:"is_active"`
}

// Auth returns a Gin middleware that authenticates requests using the Authorization header API key.
// It checks a Redis cache first with a 5-minute TTL. On cache miss, it queries PostgreSQL.
// If Redis is down, it returns a 503 Service Unavailable error and never falls back to direct PostgreSQL query.
func Auth(hashSecret string) gin.HandlerFunc {
	return func(c *gin.Context) {
		authHeader := c.GetHeader("Authorization")
		if authHeader == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Missing Authorization header"})
			c.Abort()
			return
		}

		// Support "Bearer <key>" or raw key format
		var rawKey string
		if strings.HasPrefix(authHeader, "Bearer ") {
			rawKey = strings.TrimPrefix(authHeader, "Bearer ")
		} else {
			rawKey = authHeader
		}

		if rawKey == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid Authorization header format"})
			c.Abort()
			return
		}

		// Compute key hash using HMAC-SHA256
		h := hmac.New(sha256.New, []byte(hashSecret))
		h.Write([]byte(rawKey))
		keyHash := hex.EncodeToString(h.Sum(nil))

		cacheKey := fmt.Sprintf("auth:key:%s", keyHash)
		ctx := c.Request.Context()

		// Verify Redis client is configured
		if chugredis.Client == nil {
			log.Println("auth middleware error: Redis client is not initialized")
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Service temporarily unavailable"})
			c.Abort()
			return
		}

		// 1. Attempt lookup in Redis
		cachedVal, err := chugredis.Client.Get(ctx, cacheKey).Result()
		if err != nil && err != goredis.Nil {
			// Redis is unavailable (network issue, server down, etc.)
			log.Printf("auth middleware: Redis unavailable error: %v", err)
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Service temporarily unavailable"})
			c.Abort()
			return
		}

		if err == nil {
			// Cache Hit!
			var info CachedKeyInfo
			if err := json.Unmarshal([]byte(cachedVal), &info); err != nil {
				log.Printf("auth middleware: failed to unmarshal cached key info: %v", err)
				// If cache is corrupted, proceed to database query (since Redis itself is UP)
			} else {
				if !info.IsActive {
					c.JSON(http.StatusUnauthorized, gin.H{"error": "Revoked API key"})
					c.Abort()
					return
				}

				// Attach developer context to Gin context for downstream handlers
				c.Set("developer_id", info.DeveloperID)
				c.Set("api_key_id", info.APIKeyID)
				c.Next()
				return
			}
		}

		// 2. Cache Miss - Query PostgreSQL
		// Note: We only reach this because Redis is verified as UP (returned goredis.Nil, not a connection error).
		if db.DB == nil {
			log.Println("auth middleware error: PostgreSQL DB client is not initialized")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			c.Abort()
			return
		}

		var info CachedKeyInfo
		err = db.DB.QueryRow(`
			SELECT id, developer_id, is_active 
			FROM api_keys 
			WHERE key_hash = $1
		`, keyHash).Scan(&info.APIKeyID, &info.DeveloperID, &info.IsActive)

		if err == sql.ErrNoRows {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid API key"})
			c.Abort()
			return
		} else if err != nil {
			log.Printf("auth middleware: database query error: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			c.Abort()
			return
		}

		// Cache the key information in Redis with a 5-minute TTL
		infoBytes, marshalErr := json.Marshal(info)
		if marshalErr == nil {
			setErr := chugredis.Client.Set(ctx, cacheKey, infoBytes, 5*time.Minute).Err()
			if setErr != nil {
				log.Printf("auth middleware: failed to cache key info to Redis: %v", setErr)
				// Since Redis failed to write, treat Redis as unavailable now to satisfy the 503 requirement
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Service temporarily unavailable"})
				c.Abort()
				return
			}
		}

		if !info.IsActive {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Revoked API key"})
			c.Abort()
			return
		}

		// Attach context and proceed
		c.Set("developer_id", info.DeveloperID)
		c.Set("api_key_id", info.APIKeyID)
		c.Next()
	}
}
