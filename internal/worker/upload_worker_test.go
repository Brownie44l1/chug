package worker

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHandleUploadJob(t *testing.T) {
	t.Run("Valid Job ID", func(t *testing.T) {
		payload, err := json.Marshal(map[string]string{"job_id": "test-job-123"})
		require.NoError(t, err)

		task := asynq.NewTask("upload:job", payload)
		err = HandleUploadJob(context.Background(), task)
		assert.NoError(t, err)
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
