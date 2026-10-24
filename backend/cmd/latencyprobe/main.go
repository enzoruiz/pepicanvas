// Command latencyprobe is the TE-010 remote render latency harness. It serves
// one synthetic canvas on a real realtime.Hub over a loopback address only:
//
//	latencyprobe -listen <loopback-ip:port> -static <dir>
//
//	GET  /probe/ping      200 {"serverAtNs":n}, Cache-Control: no-store
//	POST /probe/commands  {"operationId","objectId","kind":"set|delete","payload":{…}}
//	                      200 {"outcome":"accepted","sequence","acceptedAtNs","correlationId"},
//	                      409 {"outcome":"rejected","reason"}, 400 malformed, 503 closed
//	GET  /probe/events    SSE: "ready" {} first, then one "canvas" event per
//	                      accepted command in sequence order
//	GET  /                static files from <dir>; /probe/* routes take precedence
//
// Server times are nanoseconds on the monotonic clock since process start.
// It prints its base URL on stdout once listening. On SIGINT or SIGTERM it
// disconnects the streams, shuts the HTTP server down and then closes the
// hub. Exit status is 0 after a clean shutdown, 1 on a runtime failure and 2
// on a usage error, a non-loopback -listen or an unusable -static directory.
// Command payloads are never logged.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

const (
	usage           = "usage: latencyprobe -listen <loopback-ip:port> -static <dir>"
	shutdownTimeout = 10 * time.Second
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run serves until ctx ends, then shuts down in order: streams and HTTP
// server first, the hub last.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("latencyprobe", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	listen := flags.String("listen", "", "loopback IP address and port")
	static := flags.String("static", "", "directory served at /")
	if err := flags.Parse(args); err != nil || *listen == "" || *static == "" || flags.NArg() != 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	if err := validateListen(*listen); err != nil {
		fmt.Fprintf(stderr, "latencyprobe: -listen must be a loopback IP address and port (127.0.0.0/8 or ::1): %v\n", err)
		return 2
	}
	p, err := newProbe(options{static: *static})
	if err != nil {
		fmt.Fprintf(stderr, "latencyprobe: -static: %v\n", err)
		return 2
	}

	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintf(stderr, "latencyprobe: listen: %v\n", err)
		closeHub(p, stderr)
		return 1
	}
	server := &http.Server{Handler: p, ReadHeaderTimeout: 5 * time.Second}
	server.RegisterOnShutdown(p.closeStreams)
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	fmt.Fprintf(stdout, "latencyprobe: listening on http://%s\n", listener.Addr())

	code := 0
	select {
	case <-ctx.Done():
	case err := <-served:
		fmt.Fprintf(stderr, "latencyprobe: serve: %v\n", err)
		code = 1
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		fmt.Fprintf(stderr, "latencyprobe: shutdown: %v\n", err)
		code = 1
	}
	if err := p.Close(shutdownCtx); err != nil {
		fmt.Fprintf(stderr, "latencyprobe: close hub: %v\n", err)
		code = 1
	}
	return code
}

func closeHub(p *probe, stderr io.Writer) {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := p.Close(ctx); err != nil {
		fmt.Fprintf(stderr, "latencyprobe: close hub: %v\n", err)
	}
}

// validateListen accepts only a literal loopback IP and a numeric port, so
// the harness never listens beyond the local machine.
func validateListen(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return fmt.Errorf("invalid port %q", port)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("%q is not an IP address", host)
	}
	if !ip.IsLoopback() {
		return errors.New(ip.String() + " is not a loopback address")
	}
	return nil
}
