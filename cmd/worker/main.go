package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hibiken/asynq"
	"github.com/robfig/cron/v3"

	"github.com/Brownie44l1/chug/internal/config"
	"github.com/Brownie44l1/chug/internal/db"
	"github.com/Brownie44l1/chug/internal/queue"
	"github.com/Brownie44l1/chug/internal/worker"
)

func main() {
	cfg := config.Load()

	// Initialize database connection (needed for Epic 3 processing)
	database, err := db.Connect(cfg.PostgresURL)
	if err != nil {
		log.Fatalf("worker: failed to connect to PostgreSQL: %v", err)
	}
	defer func() {
		if err := database.Close(); err != nil {
			log.Printf("worker: error closing database connection: %v", err)
		}
	}()

	// Initialize queue client for rescheduling webhook retries
	if err := queue.Init(cfg.RedisURL, cfg.QueueName); err != nil {
		log.Fatalf("worker: failed to initialize queue client: %v", err)
	}
	defer func() {
		if err := queue.Close(); err != nil {
			log.Printf("worker: error closing queue client: %v", err)
		}
	}()

	// Parse Redis URL for Asynq client options
	redisOpt, err := asynq.ParseRedisURI(cfg.RedisURL)
	if err != nil {
		log.Fatalf("worker: failed to parse Redis URL: %v", err)
	}

	// Create Asynq server with custom ErrorHandler and RetryDelayFunc
	srv := asynq.NewServer(
		redisOpt,
		asynq.Config{
			Concurrency: cfg.WorkerConcurrency,
			Queues: map[string]int{
				cfg.QueueName: 1,
			},
			ErrorHandler:   asynq.ErrorHandlerFunc(worker.CustomErrorHandler),
			RetryDelayFunc: worker.CustomRetryDelay,
		},
	)

	// Register Task Handlers
	mux := asynq.NewServeMux()
	mux.HandleFunc("upload:job", worker.HandleUploadJob)
	mux.HandleFunc("webhook:retry", worker.HandleWebhookRetry)

	// Start Asynq server in a background goroutine
	go func() {
		log.Printf("worker: starting background queue worker (concurrency: %d, queue: %s)", cfg.WorkerConcurrency, cfg.QueueName)
		if err := srv.Run(mux); err != nil {
			log.Fatalf("worker: server run failed: %v", err)
		}
	}()

	// Initialize and start cron scheduler for TTL cleanup of expired failed jobs (runs hourly)
	c := cron.New()
	_, err = c.AddFunc("@hourly", func() {
		log.Println("cron: running cleanup of expired failed jobs")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := worker.CleanExpiredFailedJobs(ctx, database); err != nil {
			log.Printf("cron error: failed to clean expired jobs: %v", err)
		}
	})
	if err != nil {
		log.Fatalf("worker: failed to schedule cron job: %v", err)
	}
	c.Start()
	defer c.Stop()

	// Handle Graceful Shutdown signals
	stopChan := make(chan os.Signal, 1)
	signal.Notify(stopChan, syscall.SIGINT, syscall.SIGTERM)

	// Block until a signal is received
	sig := <-stopChan
	log.Printf("worker: received signal %v, shutting down server gracefully...", sig)

	// Stop accepting new tasks and wait for in-flight tasks to complete
	srv.Shutdown()
	log.Println("worker: stopped successfully")
}
