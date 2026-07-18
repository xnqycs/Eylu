package main

import (
	"context"
	"os"

	"Eylu/internal/app"
)

func main() {
	os.Exit(app.Execute(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
