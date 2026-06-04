package worker

import (
	"context"
	"encoding/json"
	"errors"
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

type mockUploader struct {
	uploadFunc func(ctx context.Context, input storage.UploadInput) (*storage.UploadOutput, error)
}

func (m *mockUploader) Upload(ctx context.Context, input storage.UploadInput) (*storage.UploadOutput, error) {
	if m.uploadFunc != nil {
		return m.uploadFunc(ctx, input)
	}
	return &storage.UploadOutput{URL: "https://test.r2.cloudflarestorage.com/" + input.Key}, nil
}

func TestHandleUploadJob(t *testing.T) {
	cfg := config.Load()
	if cfg.PostgresURL == "" {
		t.Skip("Skipping worker tests: database not configured")
	}

	pgDB, err := db.Connect(cfg.PostgresURL)
	require.NoError(t, err)
	defer pgDB.Close()

	// Seed test data setup
	var devID string
	err = pgDB.QueryRow(`
		INSERT INTO developers (email, hashed_password)
		VALUES ($1, $2)
		ON CONFLICT (email) DO UPDATE SET email = EXCLUDED.email
		RETURNING id
	`, "test-worker-dev@example.com", "testpass").Scan(&devID)
	require.NoError(t, err)

	var keyID string
	err = pgDB.QueryRow(`
		INSERT INTO api_keys (developer_id, key_hash, label, is_active)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT DO NOTHING
		RETURNING id
	`, devID, "dummyhashworker", "test-worker-key", true).Scan(&keyID)
	if err != nil {
		// key might already exist, fetch it
		err = pgDB.QueryRow("SELECT id FROM api_keys WHERE developer_id = $1 LIMIT 1", devID).Scan(&keyID)
		require.NoError(t, err)
	}

	t.Run("Valid Job Flow", func(t *testing.T) {
		// 1. Insert a job in the database
		var jobID string
		err = pgDB.QueryRow(`
			INSERT INTO upload_jobs (developer_id, api_key_id, status, file_name, file_size_bytes, mime_type, checksum, max_retries)
			VALUES ($1, $2, 'pending', 'test.png', 10, 'image/png', 'checksum123', 3)
			RETURNING id
		`, devID, keyID).Scan(&jobID)
		require.NoError(t, err)

		// 2. Create the temp file
		tempDir := "tmp/uploads"
		err = os.MkdirAll(tempDir, 0755)
		require.NoError(t, err)
		tempFilePath := filepath.Join(tempDir, jobID)
		err = os.WriteFile(tempFilePath, []byte("pngdata123"), 0644)
		require.NoError(t, err)
		defer os.Remove(tempFilePath)

		// 3. Mock uploader
		uploader = &mockUploader{
			uploadFunc: func(ctx context.Context, input storage.UploadInput) (*storage.UploadOutput, error) {
				assert.Equal(t, jobID, input.Key)
				assert.Equal(t, []byte("pngdata123"), input.Data)
				assert.Equal(t, "image/png", input.MimeType)
				return &storage.UploadOutput{URL: "https://r2.com/test-job-url"}, nil
			},
		}
		defer func() { uploader = nil }()

		// 4. Run task handler
		payload, err := json.Marshal(map[string]string{"job_id": jobID})
		require.NoError(t, err)

		task := asynq.NewTask("upload:job", payload)
		err = HandleUploadJob(context.Background(), task)
		assert.NoError(t, err)

		// 5. Verify database update
		var status string
		var storageURL string
		err = pgDB.QueryRow(`
			SELECT status, storage_url FROM upload_jobs WHERE id = $1
		`, jobID).Scan(&status, &storageURL)
		require.NoError(t, err)
		assert.Equal(t, "success", status)
		assert.Equal(t, "https://r2.com/test-job-url", storageURL)

		// 6. Verify temp file was cleaned up
		_, err = os.Stat(tempFilePath)
		assert.True(t, os.IsNotExist(err))
	})

	t.Run("Cancelled Job Skipped", func(t *testing.T) {
		var jobID string
		err = pgDB.QueryRow(`
			INSERT INTO upload_jobs (developer_id, api_key_id, status, file_name, file_size_bytes, mime_type, checksum, max_retries)
			VALUES ($1, $2, 'cancelled', 'test.png', 10, 'image/png', 'checksum123', 3)
			RETURNING id
		`, devID, keyID).Scan(&jobID)
		require.NoError(t, err)

		// Mock uploader to fail if called
		uploader = &mockUploader{
			uploadFunc: func(ctx context.Context, input storage.UploadInput) (*storage.UploadOutput, error) {
				t.Fatal("uploader should not be called for cancelled job")
				return nil, errors.New("should not be called")
			},
		}
		defer func() { uploader = nil }()

		payload, err := json.Marshal(map[string]string{"job_id": jobID})
		require.NoError(t, err)

		task := asynq.NewTask("upload:job", payload)
		err = HandleUploadJob(context.Background(), task)
		assert.NoError(t, err)

		// Verify status remains cancelled
		var status string
		err = pgDB.QueryRow(`
			SELECT status FROM upload_jobs WHERE id = $1
		`, jobID).Scan(&status)
		require.NoError(t, err)
		assert.Equal(t, "cancelled", status)
	})

	t.Run("Idempotency Hit", func(t *testing.T) {
		// Pre-existing successful job with checksum 'idempotent-checksum'
		var existingJobID string
		err = pgDB.QueryRow(`
			INSERT INTO upload_jobs (developer_id, api_key_id, status, file_name, file_size_bytes, mime_type, checksum, storage_url, max_retries)
			VALUES ($1, $2, 'success', 'existing.png', 10, 'image/png', 'idempotent-checksum', 'https://r2.com/existing-url', 3)
			RETURNING id
		`, devID, keyID).Scan(&existingJobID)
		require.NoError(t, err)

		// New pending job with the same checksum
		var newJobID string
		err = pgDB.QueryRow(`
			INSERT INTO upload_jobs (developer_id, api_key_id, status, file_name, file_size_bytes, mime_type, checksum, max_retries)
			VALUES ($1, $2, 'pending', 'new.png', 10, 'image/png', 'idempotent-checksum', 3)
			RETURNING id
		`, devID, keyID).Scan(&newJobID)
		require.NoError(t, err)

		// Temp file for new job
		tempFilePath := filepath.Join("tmp/uploads", newJobID)
		_ = os.MkdirAll("tmp/uploads", 0755)
		err = os.WriteFile(tempFilePath, []byte("data"), 0644)
		require.NoError(t, err)
		defer os.Remove(tempFilePath)

		// Uploader should NOT be called
		uploader = &mockUploader{
			uploadFunc: func(ctx context.Context, input storage.UploadInput) (*storage.UploadOutput, error) {
				t.Fatal("uploader should not be called for idempotent hit")
				return nil, errors.New("should not be called")
			},
		}
		defer func() { uploader = nil }()

		payload, err := json.Marshal(map[string]string{"job_id": newJobID})
		require.NoError(t, err)

		task := asynq.NewTask("upload:job", payload)
		err = HandleUploadJob(context.Background(), task)
		assert.NoError(t, err)

		// Verify job is success and reuses the existing URL
		var status string
		var storageURL string
		err = pgDB.QueryRow(`
			SELECT status, storage_url FROM upload_jobs WHERE id = $1
		`, newJobID).Scan(&status, &storageURL)
		require.NoError(t, err)
		assert.Equal(t, "success", status)
		assert.Equal(t, "https://r2.com/existing-url", storageURL)

		// Verify temp file was cleaned up
		_, err = os.Stat(tempFilePath)
		assert.True(t, os.IsNotExist(err))
	})

	t.Run("R2 Upload Failure Returns Error", func(t *testing.T) {
		var jobID string
		err = pgDB.QueryRow(`
			INSERT INTO upload_jobs (developer_id, api_key_id, status, file_name, file_size_bytes, mime_type, checksum, max_retries)
			VALUES ($1, $2, 'pending', 'fail.png', 10, 'image/png', 'checksumfail', 3)
			RETURNING id
		`, devID, keyID).Scan(&jobID)
		require.NoError(t, err)

		tempFilePath := filepath.Join("tmp/uploads", jobID)
		_ = os.MkdirAll("tmp/uploads", 0755)
		err = os.WriteFile(tempFilePath, []byte("faildata"), 0644)
		require.NoError(t, err)
		defer os.Remove(tempFilePath)

		// Mock uploader to return error
		uploader = &mockUploader{
			uploadFunc: func(ctx context.Context, input storage.UploadInput) (*storage.UploadOutput, error) {
				return nil, errors.New("r2 upload connection timed out")
			},
		}
		defer func() { uploader = nil }()

		payload, err := json.Marshal(map[string]string{"job_id": jobID})
		require.NoError(t, err)

		task := asynq.NewTask("upload:job", payload)
		err = HandleUploadJob(context.Background(), task)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "r2 upload connection timed out")
	})

	t.Run("Missing Job ID", func(t *testing.T) {
		payload, err := json.Marshal(map[string]string{"job_id": ""})
		require.NoError(t, err)

		task := asynq.NewTask("upload:job", payload)
		err = HandleUploadJob(context.Background(), task)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "missing job_id")
	})

	t.Run("Invalid JSON Payload", func(t *testing.T) {
		task := asynq.NewTask("upload:job", []byte("invalid-json"))
		err := HandleUploadJob(context.Background(), task)
		assert.Error(t, err)
	})
}

func TestCustomRetryDelay(t *testing.T) {
	task := asynq.NewTask("upload:job", nil)

	assert.Equal(t, 30*time.Second, CustomRetryDelay(1, nil, task))
	assert.Equal(t, 5*time.Minute, CustomRetryDelay(2, nil, task))
	assert.Equal(t, 30*time.Minute, CustomRetryDelay(3, nil, task))
	assert.Equal(t, 30*time.Minute, CustomRetryDelay(10, nil, task))
}

func TestCleanExpiredFailedJobs(t *testing.T) {
	cfg := config.Load()
	if cfg.PostgresURL == "" {
		t.Skip("Skipping cron tests: database not configured")
	}

	pgDB, err := db.Connect(cfg.PostgresURL)
	require.NoError(t, err)
	defer pgDB.Close()

	// Seed developer
	var devID string
	err = pgDB.QueryRow(`
		INSERT INTO developers (email, hashed_password)
		VALUES ($1, $2)
		ON CONFLICT (email) DO UPDATE SET email = EXCLUDED.email
		RETURNING id
	`, "test-cron-dev@example.com", "testpass").Scan(&devID)
	require.NoError(t, err)

	// Seed settings
	_, err = pgDB.Exec(`
		INSERT INTO developer_settings (developer_id, max_file_size_bytes, max_retries, ttl_hours)
		VALUES ($1, 1000, 3, 24)
		ON CONFLICT (developer_id) DO NOTHING
	`, devID)
	require.NoError(t, err)

	var keyID string
	err = pgDB.QueryRow(`
		INSERT INTO api_keys (developer_id, key_hash, label, is_active)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT DO NOTHING
		RETURNING id
	`, devID, "dummyhashcron", "test-cron-key", true).Scan(&keyID)
	if err != nil {
		err = pgDB.QueryRow("SELECT id FROM api_keys WHERE developer_id = $1 LIMIT 1", devID).Scan(&keyID)
		require.NoError(t, err)
	}

	// Insert failed and expired job
	var expiredJobID string
	err = pgDB.QueryRow(`
		INSERT INTO upload_jobs (developer_id, api_key_id, status, file_name, file_size_bytes, mime_type, checksum, expires_at)
		VALUES ($1, $2, 'failed', 'expired.png', 10, 'image/png', 'expiredsum', now() - interval '1 hour')
		RETURNING id
	`, devID, keyID).Scan(&expiredJobID)
	require.NoError(t, err)

	// Insert failed but NOT expired job
	var validJobID string
	err = pgDB.QueryRow(`
		INSERT INTO upload_jobs (developer_id, api_key_id, status, file_name, file_size_bytes, mime_type, checksum, expires_at)
		VALUES ($1, $2, 'failed', 'valid.png', 10, 'image/png', 'validsum', now() + interval '5 hours')
		RETURNING id
	`, devID, keyID).Scan(&validJobID)
	require.NoError(t, err)

	// Run clean routine
	err = CleanExpiredFailedJobs(context.Background(), pgDB)
	require.NoError(t, err)

	// Verify expired was deleted
	var expiredExists bool
	err = pgDB.QueryRow("SELECT EXISTS(SELECT 1 FROM upload_jobs WHERE id = $1)", expiredJobID).Scan(&expiredExists)
	require.NoError(t, err)
	assert.False(t, expiredExists, "Expired failed job should have been deleted")

	// Verify valid still exists
	var validExists bool
	err = pgDB.QueryRow("SELECT EXISTS(SELECT 1 FROM upload_jobs WHERE id = $1)", validJobID).Scan(&validExists)
	require.NoError(t, err)
	assert.True(t, validExists, "Non-expired failed job should not have been deleted")
}

func TestCustomErrorHandler(t *testing.T) {
	cfg := config.Load()
	if cfg.PostgresURL == "" {
		t.Skip("Skipping error handler tests: database not configured")
	}

	pgDB, err := db.Connect(cfg.PostgresURL)
	require.NoError(t, err)
	defer pgDB.Close()

	// Seed developer
	var devID string
	err = pgDB.QueryRow(`
		INSERT INTO developers (email, hashed_password)
		VALUES ($1, $2)
		ON CONFLICT (email) DO UPDATE SET email = EXCLUDED.email
		RETURNING id
	`, "test-err-dev@example.com", "testpass").Scan(&devID)
	require.NoError(t, err)

	// Seed settings
	_, err = pgDB.Exec(`
		INSERT INTO developer_settings (developer_id, max_file_size_bytes, max_retries, ttl_hours)
		VALUES ($1, 1000, 3, 24)
		ON CONFLICT (developer_id) DO NOTHING
	`, devID)
	require.NoError(t, err)

	var keyID string
	err = pgDB.QueryRow(`
		INSERT INTO api_keys (developer_id, key_hash, label, is_active)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT DO NOTHING
		RETURNING id
	`, devID, "dummyhasherr", "test-err-key", true).Scan(&keyID)
	if err != nil {
		err = pgDB.QueryRow("SELECT id FROM api_keys WHERE developer_id = $1 LIMIT 1", devID).Scan(&keyID)
		require.NoError(t, err)
	}

	t.Run("Non-final retry attempt does not fail job", func(t *testing.T) {
		var jobID string
		err = pgDB.QueryRow(`
			INSERT INTO upload_jobs (developer_id, api_key_id, status, file_name, file_size_bytes, mime_type, checksum, max_retries)
			VALUES ($1, $2, 'pending', 'err1.png', 10, 'image/png', 'checksumerr1', 3)
			RETURNING id
		`, devID, keyID).Scan(&jobID)
		require.NoError(t, err)

		// Mock getRetryInfo to return (0, 3) -> 1st attempt out of 3 max
		originalGetRetryInfo := getRetryInfo
		getRetryInfo = func(ctx context.Context) (int, int) {
			return 0, 3
		}
		defer func() { getRetryInfo = originalGetRetryInfo }()

		payload, err := json.Marshal(map[string]string{"job_id": jobID})
		require.NoError(t, err)

		task := asynq.NewTask("upload:job", payload)
		CustomErrorHandler(context.Background(), task, errors.New("temporary s3 error"))

		// Status should still be pending
		var status string
		err = pgDB.QueryRow("SELECT status FROM upload_jobs WHERE id = $1", jobID).Scan(&status)
		require.NoError(t, err)
		assert.Equal(t, "pending", status)
	})

	t.Run("Final retry attempt marks job as failed and stores reason", func(t *testing.T) {
		var jobID string
		err = pgDB.QueryRow(`
			INSERT INTO upload_jobs (developer_id, api_key_id, status, file_name, file_size_bytes, mime_type, checksum, max_retries)
			VALUES ($1, $2, 'pending', 'err2.png', 10, 'image/png', 'checksumerr2', 3)
			RETURNING id
		`, devID, keyID).Scan(&jobID)
		require.NoError(t, err)

		// Mock getRetryInfo to return (2, 3) -> 3rd attempt out of 3 max (retries exhausted)
		originalGetRetryInfo := getRetryInfo
		getRetryInfo = func(ctx context.Context) (int, int) {
			return 2, 3
		}
		defer func() { getRetryInfo = originalGetRetryInfo }()

		payload, err := json.Marshal(map[string]string{"job_id": jobID})
		require.NoError(t, err)

		task := asynq.NewTask("upload:job", payload)
		CustomErrorHandler(context.Background(), task, errors.New("permanent R2 connection timeout"))

		// Status should be failed with failure reason and expires_at populated
		var status string
		var failureReason string
		var expiresAt time.Time
		err = pgDB.QueryRow("SELECT status, failure_reason, expires_at FROM upload_jobs WHERE id = $1", jobID).Scan(&status, &failureReason, &expiresAt)
		require.NoError(t, err)
		assert.Equal(t, "failed", status)
		assert.Equal(t, "permanent R2 connection timeout", failureReason)
		assert.True(t, expiresAt.After(time.Now()))
	})
}
