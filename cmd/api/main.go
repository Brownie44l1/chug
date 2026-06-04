package main

import (
	"fmt"
	"log"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/Brownie44l1/chug/internal/config"
	"github.com/Brownie44l1/chug/internal/db"
	"github.com/Brownie44l1/chug/internal/handler"
	"github.com/Brownie44l1/chug/internal/middleware"
	"github.com/Brownie44l1/chug/internal/queue"
	"github.com/Brownie44l1/chug/internal/redis"
)

func main() {
	cfg := config.Load()

	// Connect to PostgreSQL
	database, err := db.Connect(cfg.PostgresURL)
	if err != nil {
		log.Fatalf("failed to connect to PostgreSQL: %v", err)
	}
	defer func() {
		if err := database.Close(); err != nil {
			log.Printf("error closing database connection: %v", err)
		}
	}()

	// Connect to Redis
	redisClient, err := redis.Connect(cfg.RedisURL)
	if err != nil {
		log.Fatalf("failed to connect to Redis: %v", err)
	}
	defer func() {
		if err := redisClient.Close(); err != nil {
			log.Printf("error closing Redis connection: %v", err)
		}
	}()

	// Initialize Background Queue
	if err := queue.Init(cfg.RedisURL, cfg.QueueName); err != nil {
		log.Fatalf("failed to initialize background queue: %v", err)
	}
	defer func() {
		if err := queue.Close(); err != nil {
			log.Printf("error closing queue client: %v", err)
		}
	}()

	// Seed default developer & key for demo authentication
	if err := db.SeedDefaultDeveloperAndKey(cfg.DefaultAPIKey, cfg.APIKeyHashSecret); err != nil {
		log.Fatalf("failed to seed default developer/key: %v", err)
	}

	r := gin.Default()

	// Public routes
	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"status": "ok",
			"env":    cfg.Env,
		})
	})

	r.POST("/auth/register", func(c *gin.Context) {
		c.JSON(http.StatusNotImplemented, gin.H{
			"error": "Registration is not enabled in demo mode. Use the pre-configured API key from .env.",
		})
	})

	uploadHandler := handler.NewUploadHandler(database)

	// Protected routes
	protected := r.Group("/")
	protected.Use(middleware.Auth(cfg.APIKeyHashSecret))
	{
		// Simple endpoint to test auth middleware
		protected.GET("/auth/verify", func(c *gin.Context) {
			devID, _ := c.Get("developer_id")
			apiKeyID, _ := c.Get("api_key_id")
			c.JSON(http.StatusOK, gin.H{
				"authenticated": true,
				"developer_id":  devID,
				"api_key_id":    apiKeyID,
			})
		})

		protected.POST("/uploads", uploadHandler.Create)
		protected.GET("/uploads/:job_id", uploadHandler.Get)
	}

	addr := fmt.Sprintf(":%s", cfg.Port)
	log.Printf("chug api starting on %s", addr)

	if err := r.Run(addr); err != nil {
		log.Fatalf("server failed to start: %v", err)
	}
}