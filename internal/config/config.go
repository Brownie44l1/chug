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
	R2AccountID       string
	R2AccessKeyID     string
	R2AccessKeySecret string
	R2BucketName      string
	R2PublicURL       string
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
		R2AccountID:       getEnv("R2_ACCOUNT_ID", ""),
		R2AccessKeyID:     getEnv("R2_ACCESS_KEY_ID", ""),
		R2AccessKeySecret: getEnv("R2_ACCESS_KEY_SECRET", ""),
		R2BucketName:      getEnv("R2_BUCKET_NAME", "chug-uploads"),
		R2PublicURL:       getEnv("R2_PUBLIC_URL", ""),
	}
}

func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}