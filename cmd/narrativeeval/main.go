package main

import (
	"flag"
	"fmt"
	"os"

	"hify/internal/narrativeeval"
)

func main() {
	truth := flag.String("reference", "", "reference relations JSONL")
	prediction := flag.String("prediction", "", "prediction snapshot JSON")
	aliases := flag.String("aliases", "", "reference aliases JSON")
	output := flag.String("output", "", "write report to this path (default stdout)")
	flag.Parse()
	if *truth == "" || *prediction == "" || *aliases == "" {
		fmt.Fprintln(os.Stderr, "reference, prediction and aliases are required")
		os.Exit(2)
	}
	w := os.Stdout
	var f *os.File
	var err error
	if *output != "" {
		f, err = os.Create(*output)
		if err != nil {
			fail(err)
		}
		defer f.Close()
		w = f
	}
	if err := narrativeeval.RunScore(*truth, *prediction, *aliases, w); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
