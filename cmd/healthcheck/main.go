package main

import (
	"net/http"
	"os"
	"time"
)

func main() {
	c := http.Client{Timeout: time.Second}
	r, err := c.Get("http://127.0.0.1:8080/readyz")
	if err != nil || r.StatusCode != 200 {
		os.Exit(1)
	}
}
