package config

import (
	"log"
	"os"
	"strconv"

	"github.com/joho/godotenv"
)

type Config struct {
	Port              string
	Env               string
	PostgresURL       string
	RedisURL          string
	APIKeyHashSecret  string
	DefaultAPIKey     string
	QueueName         string
	WorkerConcurrency int
}

func Load() *Config {
	if err := godotenv.Load(); err != nil {
		log.Println("no .env file found, reading from environment")
	}

	concurrencyStr := getEnv("WORKER_CONCURRENCY", "10")
	var concurrency int = 10
	if parsed, err := strconv.Atoi(concurrencyStr); err == nil {
		concurrency = parsed
	}

	return &Config{
		Port:              getEnv("PORT", "8080"),
		Env:               getEnv("ENV", "development"),
		PostgresURL:       getEnv("POSTGRES_URL", ""),
		RedisURL:          getEnv("REDIS_URL", ""),
		APIKeyHashSecret:  getEnv("API_KEY_HASH_SECRET", ""),
		DefaultAPIKey:     getEnv("API_KEY", ""),
		QueueName:         getEnv("QUEUE_NAME", "uploads:default"),
		WorkerConcurrency: concurrency,
	}
}

func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}