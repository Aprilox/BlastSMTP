// Command blastsmtp is a local mass-mailing console: it serves a web UI on the
// loopback interface and drives your own SMTP relay from it.
package main

import (
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/aprilox/blastsmtp/internal/mailer"
	"github.com/aprilox/blastsmtp/internal/server"
	"github.com/aprilox/blastsmtp/internal/store"
)

// version is overridden at build time:
//
//	go build -ldflags "-X main.version=1.2.3"
var version = "dev"

//go:embed all:web
var embeddedWeb embed.FS

func main() {
	var (
		host       = flag.String("host", "127.0.0.1", "interface to bind (keep it on loopback unless you know what you are doing)")
		port       = flag.Int("port", 7333, "TCP port; 0 picks a free one")
		configPath = flag.String("config", "", "path to the configuration file (default: per-user config directory)")
		noBrowser  = flag.Bool("no-browser", false, "do not open the browser automatically")
		showVer    = flag.Bool("version", false, "print the version and exit")
	)
	flag.Parse()

	if *showVer {
		fmt.Printf("blastsmtp %s (%s/%s, %s)\n", version, runtime.GOOS, runtime.GOARCH, runtime.Version())
		return
	}
	mailer.UserAgent = "BlastSMTP/" + version
	server.Version = version

	if err := run(*host, *port, *configPath, *noBrowser); err != nil {
		log.Fatalf("blastsmtp: %v", err)
	}
}

func run(host string, port int, configPath string, noBrowser bool) error {
	assets, err := fs.Sub(embeddedWeb, "web")
	if err != nil {
		return fmt.Errorf("embedded assets: %w", err)
	}

	st, err := store.New(configPath)
	if err != nil {
		return err
	}
	srv, err := server.New(assets, st)
	if err != nil {
		return err
	}

	ln, err := server.Listen(host, port)
	if err != nil {
		return fmt.Errorf("cannot listen on %s:%d: %w", host, port, err)
	}
	url := fmt.Sprintf("http://%s/?token=%s", ln.Addr().String(), srv.Token())

	httpSrv := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
	}

	fmt.Printf("\n  BlastSMTP %s\n", version)
	fmt.Printf("  Console   %s\n", url)
	fmt.Printf("  Config    %s\n", st.Path())
	fmt.Printf("  Ctrl+C to quit\n\n")

	if !noBrowser {
		openBrowser(url)
	}

	errc := make(chan error, 1)
	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	select {
	case err := <-errc:
		return err
	case <-stop:
		fmt.Println("\n  shutting down...")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return httpSrv.Shutdown(ctx)
}

// openBrowser is best-effort: failing to launch one is not fatal, the URL is
// printed above anyway.
func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		return
	}
	go func() { _ = cmd.Wait() }()
}
