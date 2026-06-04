package worker

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Brownie44l1/chug/internal/config"
	"github.com/Brownie44l1/chug/internal/db"
	"github.com/Brownie44l1/chug/internal/storage"
)

func TestWebhookFiring(t *testing.T) {
	cfg := config.Load()
	if cfg.PostgresURL == "" {
		t.Skip("Skipping webhook worker tests: database not configured")
	}

	pgDB, err := db.Connect(cfg.PostgresURL)
	require.NoError(t, err)
	defer pgDB.Close()

	// Assign to global DB so worker can use it
	db.DB = pgDB

	// Seed test data
	var devID string
	err = pgDB.QueryRow(`
		INSERT INTO developers (email, hashed_password)
		VALUES ($1, $2)
		ON CONFLICT (email) DO UPDATE SET email = EXCLUDED.email
		RETURNING id
	`, "test-webhook-worker-dev@example.com", "testpass").Scan(&devID)
	require.NoError(t, err)

	var keyID string
	err = pgDB.QueryRow(`
		INSERT INTO api_keys (developer_id, key_hash, label, is_active)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT DO NOTHING
		RETURNING id
	`, devID, "dummyhashwebhookworker", "test-webhook-worker-key", true).Scan(&keyID)
	if err != nil {
		err = pgDB.QueryRow("SELECT id FROM api_keys WHERE developer_id = $1 LIMIT 1", devID).Scan(&keyID)
		require.NoError(t, err)
	}

	// Clean developer settings
	_, err = pgDB.Exec(`
		INSERT INTO developer_settings (developer_id, max_file_size_bytes, max_retries, ttl_hours)
		VALUES ($1, 100, 3, 24)
		ON CONFLICT (developer_id) DO UPDATE SET max_retries = 3, ttl_hours = 24
	`, devID)
	require.NoError(t, err)

	// Clean webhook endpoints and deliveries
	_, _ = pgDB.Exec("DELETE FROM webhook_deliveries")
	_, _ = pgDB.Exec("DELETE FROM webhook_endpoints WHERE developer_id = $1", devID)

	// Create test server to receive the webhook
	receivedSignal := make(chan bool, 1)
	var receivedPayload map[string]interface{}
	var receivedSignature string

	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.Unmarshal(body, &receivedPayload)
		receivedSignature = r.Header.Get("X-Webhook-Signature")
		w.WriteHeader(http.StatusOK)
		receivedSignal <- true
	}))
	defer testServer.Close()

	// Register webhook endpoint in DB
	webhookSecret := "supersecretkey"
	var webhookEndpointID string
	err = pgDB.QueryRow(`
		INSERT INTO webhook_endpoints (developer_id, url, secret, is_active)
		VALUES ($1, $2, $3, true)
		RETURNING id
	`, devID, testServer.URL, webhookSecret).Scan(&webhookEndpointID)
	require.NoError(t, err)

	t.Run("Webhook Fire on Job Success", func(t *testing.T) {
		// Insert job
		var jobID string
		err = pgDB.QueryRow(`
			INSERT INTO upload_jobs (developer_id, api_key_id, status, file_name, file_size_bytes, mime_type, checksum, max_retries)
			VALUES ($1, $2, 'pending', 'webhook_success.png', 10, 'image/png', 'checksum_web_success', 3)
			RETURNING id
		`, devID, keyID).Scan(&jobID)
		require.NoError(t, err)

		// Create temp file
		tempDir := "tmp/uploads"
		err = os.MkdirAll(tempDir, 0755)
		require.NoError(t, err)
		tempFilePath := filepath.Join(tempDir, jobID)
		err = os.WriteFile(tempFilePath, []byte("pngdata123"), 0644)
		require.NoError(t, err)
		defer os.Remove(tempFilePath)

		// Mock Uploader
		uploader = &mockUploader{
			uploadFunc: func(ctx context.Context, input storage.UploadInput) (*storage.UploadOutput, error) {
				return &storage.UploadOutput{URL: "https://r2.com/test-webhook-success"}, nil
			},
		}
		defer func() { uploader = nil }()

		// Run task handler
		payload, err := json.Marshal(map[string]string{"job_id": jobID})
		require.NoError(t, err)

		task := asynq.NewTask("upload:job", payload)
		err = HandleUploadJob(context.Background(), task)
		require.NoError(t, err)

		// Wait for webhook signal to reach the test HTTP server
		select {
		case <-receivedSignal:
			// Success
		case <-time.After(3 * time.Second):
			t.Fatal("Timeout waiting for webhook callback")
		}

		// Verify Webhook Payload content
		assert.Equal(t, jobID, receivedPayload["job_id"])
		assert.Equal(t, "success", receivedPayload["status"])
		assert.Equal(t, "https://r2.com/test-webhook-success", receivedPayload["storage_url"])

		// Verify signature
		bodyBytes, err := json.Marshal(receivedPayload)
		require.NoError(t, err)

		h := hmac.New(sha256.New, []byte(webhookSecret))
		h.Write(bodyBytes)
		assert.Len(t, receivedSignature, 64)

		// Poll the database to avoid race condition with async worker DB updates
		var dbStatus string
		var dbResponseStatus sql.NullInt64
		var dbAttemptCount int
		for i := 0; i < 20; i++ {
			err = pgDB.QueryRow(`
				SELECT status, response_status, attempt_count
				FROM webhook_deliveries
				WHERE upload_job_id = $1 AND webhook_endpoint_id = $2
			`, jobID, webhookEndpointID).Scan(&dbStatus, &dbResponseStatus, &dbAttemptCount)
			if err == nil && dbStatus != "pending" {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		require.NoError(t, err)

		assert.Equal(t, "delivered", dbStatus)
		assert.Equal(t, int64(200), dbResponseStatus.Int64)
		assert.Equal(t, 1, dbAttemptCount)
	})

	t.Run("Webhook Fire on Job Failure", func(t *testing.T) {
		// Clear channel
		select {
		case <-receivedSignal:
		default:
		}

		// Insert job
		var jobID string
		err = pgDB.QueryRow(`
			INSERT INTO upload_jobs (developer_id, api_key_id, status, file_name, file_size_bytes, mime_type, checksum, max_retries)
			VALUES ($1, $2, 'pending', 'webhook_failed.png', 10, 'image/png', 'checksum_web_failed', 1)
			RETURNING id
		`, devID, keyID).Scan(&jobID)
		require.NoError(t, err)

		// Mock retry info to act as final retry
		originalGetRetryInfo := getRetryInfo
		getRetryInfo = func(ctx context.Context) (int, int) {
			return 0, 1 // retried = 0, max = 1 (so retried >= max - 1 holds true)
		}
		defer func() { getRetryInfo = originalGetRetryInfo }()

		// Run error handler directly
		taskPayload, err := json.Marshal(map[string]string{"job_id": jobID})
		require.NoError(t, err)
		task := asynq.NewTask("upload:job", taskPayload)

		CustomErrorHandler(context.Background(), task, errors.New("simulated failure"))

		// Wait for webhook signal
		select {
		case <-receivedSignal:
			// Success
		case <-time.After(3 * time.Second):
			t.Fatal("Timeout waiting for webhook callback")
		}

		// Verify Webhook Payload content
		assert.Equal(t, jobID, receivedPayload["job_id"])
		assert.Equal(t, "failed", receivedPayload["status"])
		assert.Equal(t, "simulated failure", receivedPayload["failure_reason"])

		// Poll the database to avoid race condition with async worker DB updates
		var dbStatus string
		var dbResponseStatus sql.NullInt64
		var dbAttemptCount int
		for i := 0; i < 20; i++ {
			err = pgDB.QueryRow(`
				SELECT status, response_status, attempt_count
				FROM webhook_deliveries
				WHERE upload_job_id = $1 AND webhook_endpoint_id = $2
			`, jobID, webhookEndpointID).Scan(&dbStatus, &dbResponseStatus, &dbAttemptCount)
			if err == nil && dbStatus != "pending" {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		require.NoError(t, err)

		assert.Equal(t, "delivered", dbStatus)
		assert.Equal(t, int64(200), dbResponseStatus.Int64)
		assert.Equal(t, 1, dbAttemptCount)
	})
}
