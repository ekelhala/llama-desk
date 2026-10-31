package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ekelhala/llama-desk/internal/desk"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8090", "HTTP listen address")
	models := flag.String("models", "./models", "model directory")
	binary := flag.String("llama-server", "llama-server", "native llama-server executable")
	data := flag.String("data", "./data", "job history and launch settings directory")
	parallel := flag.Int("downloads", 3, "maximum parallel downloads")
	flag.Parse()
	app, err := desk.New(*models, *binary, *data, *parallel)
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	server := &http.Server{Addr: *listen, Handler: app.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		_ = server.Shutdown(shutdown)
		app.Close()
	}()
	log.Printf("llama-desk listening at http://%s (no authentication)", *listen)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
	// Wait for child processes and download workers to finish on shutdown.
	<-ctx.Done()
	app.Close()
}
