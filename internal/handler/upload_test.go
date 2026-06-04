package handler

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Brownie44l1/chug/internal/config"
	"github.com/Brownie44l1/chug/internal/db"
	"github.com/Brownie44l1/chug/internal/middleware"
	"github.com/Brownie44l1/chug/internal/queue"
	"github.com/Brownie44l1/chug/internal/redis"
)

func TestUploadHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cfg := config.Load()
	if cfg.PostgresURL == "" || cfg.RedisURL == "" {
		t.Skip("Skipping handler tests: database or redis url not configured")
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
	`, "test-handler-dev@example.com", "testpass").Scan(&devID)
	require.NoError(t, err)

	// Seed developer settings with 100 bytes limit for testing file too large
	_, err = pgDB.Exec(`
		INSERT INTO developer_settings (developer_id, max_file_size_bytes, max_retries, ttl_hours)
		VALUES ($1, 100, 3, 24)
		ON CONFLICT (developer_id) DO UPDATE SET max_file_size_bytes = 100
	`, devID)
	require.NoError(t, err)

	validKey := "chug_handler_valid_key"
	hashedValidKey := hashKey(validKey)
	var validKeyID string

	// Clean database state to avoid duplicate key/FK issues
	_, _ = pgDB.Exec("DELETE FROM upload_jobs WHERE developer_id = $1", devID)
	_, _ = pgDB.Exec("DELETE FROM api_keys WHERE key_hash = $1 OR developer_id = $2", hashedValidKey, devID)
	_ = rdb.Del(context.Background(), "auth:key:"+hashedValidKey)

	err = pgDB.QueryRow(`
		INSERT INTO api_keys (developer_id, key_hash, label, is_active)
		VALUES ($1, $2, $3, $4)
		RETURNING id
	`, devID, hashedValidKey, "test-valid", true).Scan(&validKeyID)
	require.NoError(t, err)

	// Setup Router
	r := gin.New()
	uploadHandler := NewUploadHandler(pgDB)

	r.POST("/uploads", middleware.Auth(hashSecret), uploadHandler.Create)

	// Mock file data
	pngBytes := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x01, 0x02}
	h := sha256.New()
	h.Write(pngBytes)
	pngChecksum := hex.EncodeToString(h.Sum(nil))

	// Helper to create multipart request
	createMultipartRequest := func(fileBytes []byte, filename, checksum string) (*http.Request, error) {
		body := &bytes.Buffer{}
		writer := multipart.NewWriter(body)

		part, err := writer.CreateFormFile("file", filename)
		if err != nil {
			return nil, err
		}
		_, err = part.Write(fileBytes)
		if err != nil {
			return nil, err
		}

		if checksum != "" {
			err = writer.WriteField("checksum", checksum)
			if err != nil {
				return nil, err
			}
		}

		err = writer.Close()
		if err != nil {
			return nil, err
		}

		req, err := http.NewRequest("POST", "/uploads", body)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", writer.FormDataContentType())
		return req, nil
	}

	t.Run("Unauthorized Request", func(t *testing.T) {
		req, err := createMultipartRequest(pngBytes, "test.png", pngChecksum)
		require.NoError(t, err)

		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("Valid File Upload", func(t *testing.T) {
		originalEnqueue := queue.EnqueueJob
		queue.EnqueueJob = func(jobID string) error {
			return nil
		}
		defer func() { queue.EnqueueJob = originalEnqueue }()

		req, err := createMultipartRequest(pngBytes, "test.png", pngChecksum)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+validKey)

		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		assert.Equal(t, http.StatusAccepted, w.Code)

		var res map[string]string
		err = json.Unmarshal(w.Body.Bytes(), &res)
		require.NoError(t, err)
		jobID := res["job_id"]
		assert.NotEmpty(t, jobID)

		// Verify job record in DB
		var status string
		var dbChecksum string
		var size int64
		err = pgDB.QueryRow(`
			SELECT status, checksum, file_size_bytes
			FROM upload_jobs
			WHERE id = $1
		`, jobID).Scan(&status, &dbChecksum, &size)
		require.NoError(t, err)
		assert.Equal(t, "pending", status)
		assert.Equal(t, pngChecksum, dbChecksum)
		assert.Equal(t, int64(len(pngBytes)), size)

		// Verify temp file exists
		tempFilePath := filepath.Join("tmp/uploads", jobID)
		defer os.Remove(tempFilePath)
		_, err = os.Stat(tempFilePath)
		assert.NoError(t, err)

		savedBytes, err := os.ReadFile(tempFilePath)
		require.NoError(t, err)
		assert.Equal(t, pngBytes, savedBytes)
	})

	t.Run("File Too Large (exceeds developer settings)", func(t *testing.T) {
		largePNG := make([]byte, 120)
		copy(largePNG[:8], pngBytes[:8]) // Keep PNG magic bytes

		req, err := createMultipartRequest(largePNG, "large.png", "")
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+validKey)

		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Contains(t, w.Body.String(), "FILE_TOO_LARGE")
	})

	t.Run("Checksum Mismatch", func(t *testing.T) {
		req, err := createMultipartRequest(pngBytes, "test.png", "incorrect_checksum")
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+validKey)

		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Contains(t, w.Body.String(), "CHECKSUM_MISMATCH")
	})

	t.Run("Invalid File Type (txt)", func(t *testing.T) {
		txtBytes := []byte("Hello this is a simple text file.")
		req, err := createMultipartRequest(txtBytes, "test.txt", "")
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+validKey)

		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Contains(t, w.Body.String(), "INVALID_FILE_TYPE")
	})

	t.Run("Enqueue Fails -> Marked Failed in DB and clean up file", func(t *testing.T) {
		originalEnqueue := queue.EnqueueJob
		queue.EnqueueJob = func(jobID string) error {
			return fmt.Errorf("redis connection error")
		}
		defer func() { queue.EnqueueJob = originalEnqueue }()

		req, err := createMultipartRequest(pngBytes, "test.png", pngChecksum)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+validKey)

		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		assert.Equal(t, http.StatusServiceUnavailable, w.Code)

		var jobID string
		var status string
		var failureReason string
		err = pgDB.QueryRow(`
			SELECT id, status, failure_reason
			FROM upload_jobs
			WHERE developer_id = $1
			ORDER BY created_at DESC
			LIMIT 1
		`, devID).Scan(&jobID, &status, &failureReason)
		require.NoError(t, err)

		assert.Equal(t, "failed", status)
		assert.Contains(t, failureReason, "Failed to enqueue job: redis connection error")

		// Verify temp file does NOT exist
		tempFilePath := filepath.Join("tmp/uploads", jobID)
		_, err = os.Stat(tempFilePath)
		assert.True(t, os.IsNotExist(err))
	})
}
