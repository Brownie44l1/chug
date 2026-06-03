package main

import (
	"fmt"
	"log"

	"github.com/gin-gonic/gin"
	"github.com/Brownie44l1/chug/internal/config"
)

func main() {
	cfg := config.Load()

	r := gin.Default()

	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{
			"status": "ok",
			"env":    cfg.Env,
		})
	})

	addr := fmt.Sprintf(":%s", cfg.Port)
	log.Printf("chug api starting on %s", addr)

	if err := r.Run(addr); err != nil {
		log.Fatalf("server failed to start: %v", err)
	}
}