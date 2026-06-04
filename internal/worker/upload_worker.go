package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/hibiken/asynq"
)

// HandleUploadJob processes the "upload:job" task type.
func HandleUploadJob(ctx context.Context, t *asynq.Task) error {
	start := time.Now()

	var payload map[string]string
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		duration := time.Since(start)
		log.Printf("Processed task: job_id=unknown status=failed duration_ms=%d error=%v", duration.Milliseconds(), err)
		return fmt.Errorf("failed to unmarshal payload: %w", err)
	}

	jobID := payload["job_id"]
	if jobID == "" {
		duration := time.Since(start)
		log.Printf("Processed task: job_id=unknown status=failed duration_ms=%d error=missing job_id", duration.Milliseconds())
		return fmt.Errorf("missing job_id in task payload")
	}

	// For Ticket 3.1, we simulate processing by logging success.
	// Actual DB fetching and R2 upload happens in Ticket 3.2.
	duration := time.Since(start)
	log.Printf("Processed task: job_id=%s status=success duration_ms=%d error=nil", jobID, duration.Milliseconds())
	return nil
}
