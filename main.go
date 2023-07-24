package main

import (
	"context"

	"github.com/jonjohnsonjr/mane/trace"
	log "github.com/sirupsen/logrus"
	"github.com/wolfi-dev/wolfictl/pkg/cli"
)

func main() {
	if err := trace.Context(context.Background(), cli.New().ExecuteContext); err != nil {
		log.Fatalf("error during command execution: %v", err)
	}
}
