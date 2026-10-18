// Command privacyfixture is a test-only server that exercises the TE-003
// primitives in a real browser. It generates fresh decoy secrets on every
// start, serves a private page through httpx.Private, and exposes a
// server-side access cut. It is never part of a release build.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"log"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/enzoruiz/pepicanvas/backend/internal/platform/httpx"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/logging"
	"github.com/enzoruiz/pepicanvas/backend/internal/secret"
)

type decoys struct {
	Invitation    string `json:"invitation"`
	Session       string `json:"session"`
	OBSCredential string `json:"obs_credential"`
	MediaURL      string `json:"media_url"`
	PrivateMarker string `json:"private_marker"`
}

func newDecoys() (decoys, error) {
	gen := func(k secret.Kind) (string, error) {
		v, err := secret.Generate(k)
		return v.Reveal(), err
	}
	inv, err := gen(secret.KindInvitationToken)
	if err != nil {
		return decoys{}, err
	}
	ses, err := gen(secret.KindSessionToken)
	if err != nil {
		return decoys{}, err
	}
	obs, err := gen(secret.KindOBSCredential)
	if err != nil {
		return decoys{}, err
	}
	// The marker reuses the session entropy so it rotates with the decoys.
	return decoys{
		Invitation:    inv,
		Session:       ses,
		OBSCredential: obs,
		MediaURL:      "https://storage.invalid/m/1.png?X-Amz-Signature=sig" + ses[4:20],
		PrivateMarker: "private-marker-" + obs[4:20],
	}, nil
}

var privatePage = template.Must(template.New("private").Parse(`<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><title>Private canvas</title></head>
<body>
<p id="private">{{.PrivateMarker}}</p>
<img id="third-party" alt="" src="https://third-party.invalid/pixel.png">
<a id="public" href="/">public</a>
</body>
</html>`))

const publicPage = `<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><title>Public</title></head>
<body><p id="public">public page</p><a id="open" href="/private/canvas?invite=%s">open</a></body>
</html>`

func main() {
	addr := flag.String("addr", "127.0.0.1:4174", "listen address")
	logPath := flag.String("log", "", "file that receives the JSON process log")
	flag.Parse()

	logOut := os.Stderr
	if *logPath != "" {
		if err := os.MkdirAll(filepath.Dir(*logPath), 0o700); err != nil {
			log.Fatal(err)
		}
		f, err := os.OpenFile(*logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			log.Fatal(err)
		}
		defer f.Close()
		logOut = f
	}
	logger := slog.New(logging.NewHandler(slog.NewJSONHandler(logOut, nil)))

	d, err := newDecoys()
	if err != nil {
		log.Fatal(err)
	}
	responder := httpx.Responder{Logger: logger}
	var revoked atomic.Bool

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, publicPage, d.Invitation)
	})
	mux.Handle("GET /private/canvas", httpx.Private(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if revoked.Load() {
			responder.Error(w, r, httpx.ErrUnauthorized, fmt.Errorf("session %s for %s: %w", d.Session, d.MediaURL, errors.New("access revoked")))
			return
		}
		logger.InfoContext(r.Context(), "private page served", "obs_credential", d.OBSCredential, "media", d.MediaURL)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = privatePage.Execute(w, d)
	})))
	mux.HandleFunc("POST /__fixture/cut", func(w http.ResponseWriter, _ *http.Request) {
		revoked.Store(true)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /__fixture/restore", func(w http.ResponseWriter, _ *http.Request) {
		revoked.Store(false)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /__fixture/decoys", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(d)
	})

	root := httpx.Correlate(httpx.Recover(logger, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logger.InfoContext(r.Context(), "request", "method", r.Method, "url", r.URL.String(), "cookie", r.Header.Get("Cookie"))
		mux.ServeHTTP(w, r)
	})))
	logger.Info("privacy fixture listening", "addr", *addr)
	log.Fatal(http.ListenAndServe(*addr, root))
}
