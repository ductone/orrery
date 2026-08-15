package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/ductone/orrey/internal/config"
	"github.com/ductone/orrey/internal/model"
)

var version = "dev"

func main() {
	configPath := flag.String("config", "orrery.yaml", "configuration file")
	showVersion := flag.Bool("version", false, "print version")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	if _, err := config.Load(*configPath); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	fmt.Printf("orrery %s: %d models in catalog (runtime wiring in progress)\n", version, len(model.Catalog))
}
