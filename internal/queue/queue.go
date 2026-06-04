package queue

import "errors"

var ErrEnqueueFailed = errors.New("failed to enqueue job to Redis queue")

// EnqueueJob is a mockable function pointer for enqueuing a job ID to the Asynq background workers.
// The actual Asynq queue implementation will overwrite this in Ticket 2.3.
var EnqueueJob = func(jobID string) error {
	return nil
}
