package main

import (
	"fmt"
	"os"

	"github.com/thezeeshann/build-redis/internal/redis"
)

const (
	address = ":6379"
	aofPath = "database.aof"
)

func main() {
	if err := redis.ListenAndServe(address, aofPath); err != nil {
		fmt.Fprintln(os.Stderr, "redis:", err)
		os.Exit(1)
	}
}
