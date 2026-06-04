package handler

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

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
	r.GET("/uploads/:job_id", middleware.Auth(hashSecret), uploadHandler.Get)
	r.POST("/uploads/:job_id/retry", middleware.Auth(hashSecret), uploadHandler.Retry)

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
		queue.EnqueueJob = func(jobID string, maxRetries int) error {
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
		queue.EnqueueJob = func(jobID string, maxRetries int) error {
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

	t.Run("Get Job Status - Success", func(t *testing.T) {
		// 1. Insert a mock success job record
		var successJobID string
		storageURL := "https://r2.chug.dev/test.png"
		err := pgDB.QueryRow(`
			INSERT INTO upload_jobs (developer_id, api_key_id, status, file_name, file_size_bytes, mime_type, checksum, storage_url)
			VALUES ($1, $2, 'success', 'test.png', 100, 'image/png', 'abc', $3)
			RETURNING id
		`, devID, validKeyID, storageURL).Scan(&successJobID)
		require.NoError(t, err)
		defer func() {
			_, _ = pgDB.Exec("DELETE FROM upload_jobs WHERE id = $1", successJobID)
		}()

		req, err := http.NewRequest("GET", "/uploads/"+successJobID, nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+validKey)

		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)
		var res map[string]interface{}
		err = json.Unmarshal(w.Body.Bytes(), &res)
		require.NoError(t, err)

		assert.Equal(t, successJobID, res["job_id"])
		assert.Equal(t, "success", res["status"])
		assert.Equal(t, storageURL, res["storage_url"])
		assert.Nil(t, res["failure_reason"])
		assert.Nil(t, res["expires_at"])
	})

	t.Run("Get Job Status - Failed", func(t *testing.T) {
		// 1. Insert a mock failed job record
		var failedJobID string
		failureReason := "R2 upload timed out"
		expiresAt := time.Now().Add(24 * time.Hour).UTC()
		err := pgDB.QueryRow(`
			INSERT INTO upload_jobs (developer_id, api_key_id, status, file_name, file_size_bytes, mime_type, checksum, failure_reason, expires_at)
			VALUES ($1, $2, 'failed', 'test.png', 100, 'image/png', 'abc', $3, $4)
			RETURNING id
		`, devID, validKeyID, failureReason, expiresAt).Scan(&failedJobID)
		require.NoError(t, err)
		defer func() {
			_, _ = pgDB.Exec("DELETE FROM upload_jobs WHERE id = $1", failedJobID)
		}()

		req, err := http.NewRequest("GET", "/uploads/"+failedJobID, nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+validKey)

		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)
		var res map[string]interface{}
		err = json.Unmarshal(w.Body.Bytes(), &res)
		require.NoError(t, err)

		assert.Equal(t, failedJobID, res["job_id"])
		assert.Equal(t, "failed", res["status"])
		assert.Nil(t, res["storage_url"])
		assert.Equal(t, failureReason, res["failure_reason"])
		assert.NotEmpty(t, res["expires_at"])
	})

	t.Run("Get Job Status - Not Found (unrecognised UUID)", func(t *testing.T) {
		req, err := http.NewRequest("GET", "/uploads/11111111-2222-3333-4444-555555555555", nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+validKey)

		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		assert.Equal(t, http.StatusNotFound, w.Code)
	})

	t.Run("Get Job Status - Not Found (invalid UUID format)", func(t *testing.T) {
		req, err := http.NewRequest("GET", "/uploads/invalid-uuid-format", nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+validKey)

		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		assert.Equal(t, http.StatusNotFound, w.Code)
	})

	t.Run("Get Job Status - Forbidden (belongs to different developer)", func(t *testing.T) {
		// 1. Seed another developer
		var otherDevID string
		err := pgDB.QueryRow(`
			INSERT INTO developers (email, hashed_password)
			VALUES ($1, $2)
			ON CONFLICT (email) DO UPDATE SET email = EXCLUDED.email
			RETURNING id
		`, "other-test-dev@example.com", "otherpass").Scan(&otherDevID)
		require.NoError(t, err)
		defer func() {
			_, _ = pgDB.Exec("DELETE FROM developers WHERE id = $1", otherDevID)
		}()

		// 2. Insert job owned by other developer
		var otherJobID string
		err = pgDB.QueryRow(`
			INSERT INTO upload_jobs (developer_id, api_key_id, status, file_name, file_size_bytes, mime_type, checksum)
			VALUES ($1, $2, 'pending', 'other.png', 100, 'image/png', 'abc')
			RETURNING id
		`, otherDevID, validKeyID).Scan(&otherJobID)
		require.NoError(t, err)
		defer func() {
			_, _ = pgDB.Exec("DELETE FROM upload_jobs WHERE id = $1", otherJobID)
		}()

		// 3. Try to query using validKey (which is owned by devID, NOT otherDevID)
		req, err := http.NewRequest("GET", "/uploads/"+otherJobID, nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+validKey)

		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		assert.Equal(t, http.StatusForbidden, w.Code)
	})

	t.Run("Retry Job - Job Not Found", func(t *testing.T) {
		req, err := http.NewRequest("POST", "/uploads/00000000-0000-0000-0000-000000000000/retry", nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+validKey)

		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		assert.Equal(t, http.StatusNotFound, w.Code)
	})

	t.Run("Retry Job - Forbidden (Other Developer)", func(t *testing.T) {
		// 1. Seed another developer
		var otherDevID string
		err = pgDB.QueryRow(`
			INSERT INTO developers (email, hashed_password)
			VALUES ($1, $2)
			ON CONFLICT (email) DO UPDATE SET email = EXCLUDED.email
			RETURNING id
		`, "test-other-retry-dev@example.com", "testpass").Scan(&otherDevID)
		require.NoError(t, err)

		// 2. Insert job for that developer
		var otherJobID string
		err = pgDB.QueryRow(`
			INSERT INTO upload_jobs (developer_id, api_key_id, status, file_name, file_size_bytes, mime_type, checksum)
			VALUES ($1, $2, 'failed', 'other.png', 100, 'image/png', 'xyz')
			RETURNING id
		`, otherDevID, validKeyID).Scan(&otherJobID)
		require.NoError(t, err)
		defer func() {
			_, _ = pgDB.Exec("DELETE FROM upload_jobs WHERE id = $1", otherJobID)
		}()

		req, err := http.NewRequest("POST", "/uploads/"+otherJobID+"/retry", nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+validKey)

		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		assert.Equal(t, http.StatusForbidden, w.Code)
	})

	t.Run("Retry Job - Job Expired", func(t *testing.T) {
		var expiredJobID string
		err = pgDB.QueryRow(`
			INSERT INTO upload_jobs (developer_id, api_key_id, status, file_name, file_size_bytes, mime_type, checksum, expires_at)
			VALUES ($1, $2, 'failed', 'expired.png', 10, 'image/png', 'checksumexp', now() - interval '1 hour')
			RETURNING id
		`, devID, validKeyID).Scan(&expiredJobID)
		require.NoError(t, err)
		defer func() {
			_, _ = pgDB.Exec("DELETE FROM upload_jobs WHERE id = $1", expiredJobID)
		}()

		req, err := http.NewRequest("POST", "/uploads/"+expiredJobID+"/retry", nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+validKey)

		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		assert.Equal(t, http.StatusNotFound, w.Code)
	})

	t.Run("Retry Job - Bad Request (Not Failed State)", func(t *testing.T) {
		var pendingJobID string
		err = pgDB.QueryRow(`
			INSERT INTO upload_jobs (developer_id, api_key_id, status, file_name, file_size_bytes, mime_type, checksum)
			VALUES ($1, $2, 'pending', 'pending.png', 10, 'image/png', 'checksumpen')
			RETURNING id
		`, devID, validKeyID).Scan(&pendingJobID)
		require.NoError(t, err)
		defer func() {
			_, _ = pgDB.Exec("DELETE FROM upload_jobs WHERE id = $1", pendingJobID)
		}()

		req, err := http.NewRequest("POST", "/uploads/"+pendingJobID+"/retry", nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+validKey)

		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("Retry Job - Successful Reset and Enqueue", func(t *testing.T) {
		var failedJobID string
		err = pgDB.QueryRow(`
			INSERT INTO upload_jobs (developer_id, api_key_id, status, file_name, file_size_bytes, mime_type, checksum, expires_at, failure_reason, retry_count)
			VALUES ($1, $2, 'failed', 'failed.png', 10, 'image/png', 'checksumfailtest', now() + interval '24 hours', 'connection error', 3)
			RETURNING id
		`, devID, validKeyID).Scan(&failedJobID)
		require.NoError(t, err)
		defer func() {
			_, _ = pgDB.Exec("DELETE FROM upload_jobs WHERE id = $1", failedJobID)
		}()

		// Mock queue enqueue function
		originalEnqueue := queue.EnqueueJob
		enqueuedJobID := ""
		enqueuedMaxRetries := -1
		queue.EnqueueJob = func(jobID string, maxRetries int) error {
			enqueuedJobID = jobID
			enqueuedMaxRetries = maxRetries
			return nil
		}
		defer func() { queue.EnqueueJob = originalEnqueue }()

		req, err := http.NewRequest("POST", "/uploads/"+failedJobID+"/retry", nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+validKey)

		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		assert.Equal(t, http.StatusAccepted, w.Code)
		assert.Equal(t, failedJobID, enqueuedJobID)
		assert.Equal(t, 3, enqueuedMaxRetries)

		// Verify database reset
		var status string
		var retryCount int
		var failureReason sql.NullString
		var expiresAt sql.NullTime
		err = pgDB.QueryRow(`
			SELECT status, retry_count, failure_reason, expires_at
			FROM upload_jobs
			WHERE id = $1
		`, failedJobID).Scan(&status, &retryCount, &failureReason, &expiresAt)
		require.NoError(t, err)

		assert.Equal(t, "pending", status)
		assert.Equal(t, 0, retryCount)
		assert.False(t, failureReason.Valid)
		assert.False(t, expiresAt.Valid)
	})
}
