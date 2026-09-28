package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"Eylu/internal/hostmock"
)

func main() {
	var o hostmock.Options
	flag.StringVar(&o.Engine, "engine", "", "path to the Eylu executable")
	flag.StringVar(&o.LedgerPath, "ledger", "", "path to a synthetic host ledger (one host process per file)")
	flag.BoolVar(&o.Interrupt, "interrupt", false, "interrupt during synthetic host approval")
	flag.Parse()
	o.Diagnostics = os.Stderr
	ctx, cancel := context.WithTimeout(context.Background(), hostmock.DefaultTimeout)
	defer cancel()
	report, err := hostmock.Run(ctx, o)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	_ = json.NewEncoder(os.Stdout).Encode(report)
}
