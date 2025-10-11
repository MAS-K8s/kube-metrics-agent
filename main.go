package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/prometheus/client_golang/api"
	v1 "github.com/prometheus/client_golang/api/prometheus/v1"
)

func main() {
	client, err := api.NewClient(api.Config{
		Address: "http://136.117.195.136:30980",
	})
	if err != nil {
		log.Fatalf("Error creating client: %v", err)
	}

	v1api := v1.NewAPI(client)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	query := `avg(rate(container_cpu_usage_seconds_total{namespace="default"}[2m])) by (pod)`
	result, warnings, err := v1api.Query(ctx, query, time.Now())
	if err != nil {
		log.Fatalf("Error querying Prometheus: %v", err)
	}
	if len(warnings) > 0 {
		log.Printf("Warnings: %v", warnings)
	}
	fmt.Printf("Query Result:\n%v\n", result)
}
