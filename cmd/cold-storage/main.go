package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"indicer/internal/core/jsonbridge"
	platformapi "indicer/pkg/api"
)

func main() {
	fs := flag.NewFlagSet("cold-storage", flag.ExitOnError)
	operation := fs.String("operation", "", "product operation")
	params := fs.String("params", "{}", "operation params as JSON object")
	fs.Parse(os.Args[1:])

	if *operation == "" {
		fmt.Fprintln(os.Stderr, "--operation is required")
		os.Exit(2)
	}

	var raw json.RawMessage = json.RawMessage([]byte(*params))
	request, _ := json.Marshal(map[string]any{
		"version":   jsonbridge.CurrentVersion,
		"product":   "cold_storage_product",
		"operation": *operation,
		"params":    raw,
	})
	fmt.Println(platformapi.DefaultDispatcher().DispatchJSON(context.Background(), string(request)))
}
