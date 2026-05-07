package main

import (
	"context"
	"log"
	"os"

	"github.com/TomTonic/websyncd/internal/app"
)

func main() {
	cfg, err := app.LoadConfigFromEnv()
	if err != nil {
		log.Printf("configuration error: %v", err)
		os.Exit(1)
	}

	if err := app.Run(context.Background(), cfg, log.Default()); err != nil {
		log.Printf("websyncd stopped with error: %v", err)
		os.Exit(1)
	}
}
