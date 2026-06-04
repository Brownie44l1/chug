package redis

import (
	"context"
	"fmt"
	"log"

	"github.com/redis/go-redis/v9"
)

var Client *redis.Client

// Connect opens a connection to the Redis server and pings it to verify availability.
func Connect(redisURL string) (*redis.Client, error) {
	if redisURL == "" {
		return nil, fmt.Errorf("REDIS_URL environment variable is empty")
	}

	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse redis url: %w", err)
	}

	client := redis.NewClient(opts)

	if err := client.Ping(context.Background()).Err(); err != nil {
		client.Close()
		return nil, fmt.Errorf("failed to ping redis: %w", err)
	}

	Client = client
	log.Println("successfully connected to Redis")
	return client, nil
}

// Close closes the Redis client.
func Close() error {
	if Client != nil {
		return Client.Close()
	}
	return nil
}
