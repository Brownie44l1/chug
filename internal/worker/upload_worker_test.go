package worker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

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
