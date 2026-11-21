package main

import (
	"fmt"
	"os"
	"runtime/debug"
)

func main() {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		os.Exit(1)
	}
	settings := make(map[string]string)
	for _, item := range info.Settings {
		settings[item.Key] = item.Value
	}
	if settings["vcs"] != "git" || settings["vcs.revision"] != os.Getenv("EXPECTED_COMMIT") || settings["vcs.modified"] != "false" {
		fmt.Println("walk: invalid build revision or dirty source")
		os.Exit(1)
	}
	fmt.Printf("walk: stamped %s clean\n", settings["vcs.revision"])
}
