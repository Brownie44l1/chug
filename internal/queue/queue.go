package queue

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/hibiken/asynq"
)

var ErrEnqueueFailed = errors.New("failed to enqueue job to Redis queue")

// client is the global asynq Client
var client *asynq.Client

// queueName is the target queue for upload jobs
var queueName string

// EnqueueJob is a mockable function pointer for enqueuing a job ID to the Asynq background workers.
var EnqueueJob = func(jobID string, maxRetries int) error {
	if client == nil {
		return fmt.Errorf("queue client not initialized")
	}

	payload, err := json.Marshal(map[string]string{"job_id": jobID})
	if err != nil {
		return fmt.Errorf("failed to marshal payload: %w", err)
	}

	// Task payload contains job_id only
	task := asynq.NewTask("upload:job", payload)

	errChan := make(chan error, 1)
	go func() {
		_, err := client.Enqueue(task, asynq.Queue(queueName), asynq.MaxRetry(maxRetries))
		errChan <- err
	}()

	select {
	case err := <-errChan:
		if err != nil {
			return fmt.Errorf("%w: %v", ErrEnqueueFailed, err)
		}
		return nil
	case <-time.After(2 * time.Second):
		return fmt.Errorf("%w: enqueue operation timed out", ErrEnqueueFailed)
	}
}

// Init initializes the global Asynq client using the provided Redis URL.
func Init(redisURL string, targetQueue string) error {
	opt, err := asynq.ParseRedisURI(redisURL)
	if err != nil {
		return fmt.Errorf("failed to parse Redis URL: %w", err)
	}

	// Adjust internal timeouts in the Redis connection option if it is asynq.RedisClientOpt
	if clientOpt, ok := opt.(asynq.RedisClientOpt); ok {
		clientOpt.DialTimeout = 2 * time.Second
		clientOpt.ReadTimeout = 2 * time.Second
		clientOpt.WriteTimeout = 2 * time.Second
		client = asynq.NewClient(clientOpt)
	} else {
		client = asynq.NewClient(opt)
	}

	queueName = targetQueue
	return nil
}

// Close closes the global Asynq client.
func Close() error {
	if client != nil {
		return client.Close()
	}
	return nil
}
