// Command greet prints a greeting.
package main

import (
	"flag"
	"fmt"
)

func main() {
	name := flag.String("name", "world", "who to greet")
	verbose := flag.Bool("verbose", false, "also print the time of day")
	flag.Parse()
	fmt.Println(greeting(*name, *verbose))
}

func greeting(name string, verbose bool) string {
	if verbose {
		return "Hello, " + name + "! (verbose)"
	}
	return "Hello, " + name + "!"
}
