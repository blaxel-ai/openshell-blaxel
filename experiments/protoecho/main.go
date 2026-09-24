// Reports which HTTP protocol the Blaxel proxy uses toward the workload.
package main

import (
	"fmt"
	"net/http"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

func main() {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "proto=%s te=%q ct=%q\n", r.Proto, r.Header.Get("Te"), r.Header.Get("Content-Type"))
	})
	http.ListenAndServe(":10001", h2c.NewHandler(h, &http2.Server{}))
}
