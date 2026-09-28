// host-schema writes the checked-in protocol schema; it does not execute models.
package main

import (
	"fmt"
	"os"

	"Eylu/internal/host"
)

func main() {
	if len(os.Args) != 2 && len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: host-schema <output.json> [fixtures.jsonl]")
		os.Exit(2)
	}
	data, err := host.ProtocolSchema()
	if err == nil {
		err = os.WriteFile(os.Args[1], append(data, '\n'), 0644)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if len(os.Args) == 3 {
		data, err = host.ProtocolFixtures()
		if err == nil {
			err = os.WriteFile(os.Args[2], data, 0644)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
}
