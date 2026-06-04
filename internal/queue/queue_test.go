package queue

import (
	"encoding/json"
	"testing"

	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Brownie44l1/chug/internal/config"
)

func TestEnqueueJob(t *testing.T) {
	cfg := config.Load()
	if cfg.RedisURL == "" {
		t.Skip("Skipping queue tests: Redis URL not configured")
	}

	err := Init(cfg.RedisURL, "uploads:test")
	require.NoError(t, err)
	defer Close()

	// Enqueue a job
	jobID := "test-job-uuid-1234"
	err = EnqueueJob(jobID)
	require.NoError(t, err)

	// Verify job was enqueued in Redis by reading using Asynq Inspector.
	opt, err := asynq.ParseRedisURI(cfg.RedisURL)
	require.NoError(t, err)

	var inspector *asynq.Inspector
	if clientOpt, ok := opt.(asynq.RedisClientOpt); ok {
		inspector = asynq.NewInspector(clientOpt)
	} else {
		t.Skip("Skipping queue inspection: unsupported Redis option type")
		return
	}
	defer inspector.Close()

	// Fetch pending tasks in "uploads:test" queue
	tasks, err := inspector.ListPendingTasks("uploads:test")
	require.NoError(t, err)

	found := false
	for _, task := range tasks {
		if task.Type == "upload:job" {
			var payload map[string]string
			err := json.Unmarshal(task.Payload, &payload)
			if err == nil && payload["job_id"] == jobID {
				found = true
				break
			}
		}
	}

	assert.True(t, found, "Expected to find task with jobID in pending tasks queue")

	// Clean up the queue
	_, _ = inspector.DeleteAllPendingTasks("uploads:test")
}
