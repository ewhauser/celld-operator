// Command celld-operator is the entry point for the celld fleet operator.
// Fleet reconciliation has not been implemented yet.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "celld-operator:", err)
		os.Exit(1)
	}
}

func run() error {
	fs := flag.NewFlagSet("celld-operator", flag.ContinueOnError)
	showVersion := fs.Bool("version", false, "Print version and exit")
	if err := fs.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	if *showVersion {
		fmt.Println(version)
		return nil
	}
	return errors.New("fleet reconciliation is not implemented yet")
}
