package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/hibiken/asynq"

	"github.com/Brownie44l1/chug/internal/config"
	"github.com/Brownie44l1/chug/internal/db"
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

	// Parse Redis URL for Asynq client options
	redisOpt, err := asynq.ParseRedisURI(cfg.RedisURL)
	if err != nil {
		log.Fatalf("worker: failed to parse Redis URL: %v", err)
	}

	// Create Asynq server
	srv := asynq.NewServer(
		redisOpt,
		asynq.Config{
			Concurrency: cfg.WorkerConcurrency,
			Queues: map[string]int{
				cfg.QueueName: 1,
			},
		},
	)

	// Register Task Handlers
	mux := asynq.NewServeMux()
	mux.HandleFunc("upload:job", worker.HandleUploadJob)

	// Start Asynq server in a background goroutine
	go func() {
		log.Printf("worker: starting background queue worker (concurrency: %d, queue: %s)", cfg.WorkerConcurrency, cfg.QueueName)
		if err := srv.Run(mux); err != nil {
			log.Fatalf("worker: server run failed: %v", err)
		}
	}()

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
