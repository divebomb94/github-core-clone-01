package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	os.Exit(run())
}

func run() int {
	base := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")
	if base == "" {
		fmt.Fprintln(os.Stderr, "MAKEFUN_ENV_URL is required")
		return 1
	}
	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	server := &http.Server{Handler: NewApp(os.Getenv("MAKEFUN_SHOP_TOKEN"))}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()
	client := &http.Client{Timeout: 30 * time.Second}
	body, _ := json.Marshal(map[string]int{"port": listener.Addr().(*net.TCPAddr).Port})
	delay := 250 * time.Millisecond
	for {
		response, err := client.Post(base+"/register", "application/json", bytes.NewReader(body))
		if err == nil {
			status := response.StatusCode
			response.Body.Close()
			if status == 200 || status == 409 {
				break
			}
			if status == 400 || status == 405 {
				return 1
			}
		}
		retryPause(&delay)
	}
	delay = 250 * time.Millisecond
	for {
		response, err := client.Get(base + "/done")
		if err == nil {
			var value map[string]json.RawMessage
			// Read the entire body so trailing malformed JSON is rejected as in fetch.json.
			data, readErr := io.ReadAll(response.Body)
			response.Body.Close()
			if response.StatusCode >= 200 && response.StatusCode < 300 && readErr == nil && json.Unmarshal(data, &value) == nil && string(value["done"]) == "true" {
				break
			}
		}
		retryPause(&delay)
	}
	return 0
}

func retryPause(delay *time.Duration) {
	time.Sleep(*delay)
	*delay *= 2
	if *delay > 2*time.Second {
		*delay = 2 * time.Second
	}
}
