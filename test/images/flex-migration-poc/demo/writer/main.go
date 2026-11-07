/*
Copyright 2026 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// writer appends a timestamped line to /data/log every 2s; "writer cat" prints the log.
package main

import (
	"fmt"
	"io"
	"os"
	"time"
)

const logPath = "/data/log"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "cat" {
		f, err := os.Open(logPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		_, _ = io.Copy(os.Stdout, f)
		return
	}
	name, uid := os.Getenv("POD_NAME"), os.Getenv("POD_UID")
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for {
		fmt.Fprintf(f, "%s pod=%s uid=%s\n", time.Now().UTC().Format(time.RFC3339), name, uid)
		_ = f.Sync()
		time.Sleep(2 * time.Second)
	}
}
