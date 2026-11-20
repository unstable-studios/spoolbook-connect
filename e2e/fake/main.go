// A stand-in for spoolbook's token endpoint: logs each request, answers 200.
package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
)

func main() {
	f, _ := os.OpenFile(os.Args[1], os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		auth := r.Header.Get("Authorization")
		fmt.Fprintf(f, "%s %s auth_ok=%v %s\n", r.Method, r.URL.Path, len(auth) > 20, b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"spool":{},"revision":1,"changed":true}`))
	})
	_ = http.ListenAndServe("127.0.0.1:8787", nil)
}
