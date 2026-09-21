// AI Hub phone sidecar (docs/PHONE-SCOPE.md, roadmap Phase 8, decision 10).
//
// One Tailscale node embedded with tsnet, no Tailscale app on the PC. The hub
// starts this program with the upstream address, a state folder under its data
// root and a marker token in the environment. The program joins the user's
// tailnet (the first time through a sign-in link it reports), listens on the
// tailnet with HTTPS from the tailnet's certificate, and proxies every request
// to the hub on the loopback with the marker header, so the hub knows the
// request came from a phone and asks for the phone session. Funnel, when the
// hub asks for it, publishes the same listener on the public internet.
//
// Everything the hub needs to know is written to stdout as one JSON object per
// line ("state" first). The program stops when stdin closes, so it never
// outlives the hub, and on SIGINT or SIGTERM.
package main

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/ipn"
	"tailscale.com/tsnet"
)

const version = "0.1.0"

// markerHeader carries the token the hub gave us; the hub treats a request
// with the right token as a phone request. Any copy a client sends is dropped.
const markerHeader = "X-AI-Hub-Phone"

// The hub's bridge header (server/app.ts): never forwarded from a phone.
const bridgeHeader = "X-AI-Hub-Run"

// adminDNS is where the user switches on MagicDNS and HTTPS certificates.
const adminDNS = "https://login.tailscale.com/admin/dns"

// adminMachines is where a tailnet that approves devices by hand approves
// this one.
const adminMachines = "https://login.tailscale.com/admin/machines"

// The sidecar's own log is kept to one previous copy of at most this size.
const logKeepBytes = 2 << 20

// noFunnel explains the "funnel" node attribute a tailnet policy needs
// before this node may publish to the internet (Tailscale's own short link).
const noFunnel = "https://tailscale.com/s/no-funnel"

type event struct {
	State   string `json:"state"`
	URL     string `json:"url,omitempty"`
	Address string `json:"address,omitempty"`
	Funnel  bool   `json:"funnel,omitempty"`
	Detail  string `json:"detail,omitempty"`
	Message string `json:"message,omitempty"`
}

var (
	outMu sync.Mutex
	out   = json.NewEncoder(os.Stdout)
)

func emit(e event) {
	outMu.Lock()
	defer outMu.Unlock()
	_ = out.Encode(e)
}

func fatal(message string) {
	emit(event{State: "error", Message: message})
	os.Exit(1)
}

type config struct {
	dir      string
	upstream *url.URL
	hostname string
	marker   string
	funnel   bool
	logPath  string
}

func readConfig() config {
	c := config{
		dir:      os.Getenv("AI_HUB_TAILNET_DIR"),
		hostname: os.Getenv("AI_HUB_TAILNET_HOSTNAME"),
		marker:   os.Getenv("AI_HUB_TAILNET_MARKER"),
		funnel:   os.Getenv("AI_HUB_TAILNET_FUNNEL") == "1",
		logPath:  os.Getenv("AI_HUB_TAILNET_LOG"),
	}
	if c.dir == "" {
		fatal("AI_HUB_TAILNET_DIR is not set.")
	}
	if c.hostname == "" {
		c.hostname = "ai-hub"
	}
	if len(c.marker) < 16 {
		fatal("AI_HUB_TAILNET_MARKER is missing or shorter than 16 characters.")
	}
	raw := os.Getenv("AI_HUB_TAILNET_UPSTREAM")
	if raw == "" {
		raw = "http://127.0.0.1:4310"
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.Host == "" || u.Path != "" && u.Path != "/" {
		fatal("AI_HUB_TAILNET_UPSTREAM must be an http address of the hub, such as http://127.0.0.1:4310.")
	}
	u.Path = ""
	c.upstream = u
	return c
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Println(version)
		return
	}
	c := readConfig()

	// tsnet's own log goes to a file when the hub names one, nowhere otherwise;
	// stdout is reserved for the JSON lines.
	logf := func(string, ...any) {}
	if c.logPath != "" {
		// One previous copy at most: a log that has grown past the limit is
		// moved aside and a fresh one started, so the data folder does not
		// grow without end (tsnet writes several lines a minute).
		if info, err := os.Stat(c.logPath); err == nil && info.Size() > logKeepBytes {
			_ = os.Rename(c.logPath, c.logPath+".1")
		}
		f, err := os.OpenFile(c.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err == nil {
			l := log.New(f, "", log.LstdFlags)
			logf = l.Printf
		}
	}

	srv := &tsnet.Server{
		Dir:      c.dir,
		Hostname: c.hostname,
		Logf:     logf,
		UserLogf: logf,
	}
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Stop when the hub goes away (stdin closes) or the process is asked to.
	go func() {
		_, _ = io.Copy(io.Discard, bufio.NewReader(os.Stdin))
		emit(event{State: "stopping", Detail: "stdin closed"})
		cancel()
	}()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-signals
		emit(event{State: "stopping", Detail: "signal"})
		cancel()
	}()

	emit(event{State: "starting"})
	if err := srv.Start(); err != nil {
		fatal("Tailscale could not start: " + err.Error())
	}
	lc, err := srv.LocalClient()
	if err != nil {
		fatal("Tailscale could not start: " + err.Error())
	}

	if err := waitForRunning(ctx, lc); err != nil {
		if ctx.Err() != nil {
			return
		}
		fatal(err.Error())
	}

	domain, err := waitForHTTPS(ctx, lc)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		fatal(err.Error())
	}

	// Funnel needs the "funnel" attribute in the tailnet policy. Without it
	// the hub is served on the tailnet alone and the attribute is watched;
	// when it appears this process asks to be restarted, and the next start
	// publishes.
	funnelActive := false
	if c.funnel {
		if err := funnelAllowed(ctx, lc); err != nil {
			emit(event{State: "funnel-disabled", URL: noFunnel, Message: err.Error()})
			go watchFunnel(ctx, lc, cancel)
		} else {
			funnelActive = true
		}
	}
	listener, err := listen(srv, funnelActive)
	if err != nil {
		fatal("Tailscale could not listen: " + err.Error())
	}

	// Ask for the certificate now, so the first page from a phone does not
	// wait on the issuer, and so a certificate problem is reported here.
	emit(event{State: "certificate", Detail: domain})
	certCtx, certCancel := context.WithTimeout(ctx, 90*time.Second)
	_, _, certErr := lc.CertPair(certCtx, domain)
	certCancel()
	if certErr != nil && ctx.Err() == nil {
		// Not fatal: the TLS handshake asks again on the first connection.
		emit(event{State: "certificate-pending", Detail: domain, Message: certErr.Error()})
	}

	address := "https://" + domain
	server := &http.Server{
		Handler:           proxyHandler(c, address),
		ReadHeaderTimeout: 30 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	emit(event{State: "running", Address: address, Funnel: funnelActive})
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) && ctx.Err() == nil {
		fatal("The phone listener stopped: " + err.Error())
	}
}

// waitForRunning follows the node's state until it is Running, reporting the
// sign-in link while the tailnet has not accepted this node yet.
func waitForRunning(ctx context.Context, lc *local.Client) error {
	watcher, err := lc.WatchIPNBus(ctx, ipn.NotifyInitialState)
	if err != nil {
		return errors.New("Tailscale could not start: " + err.Error())
	}
	defer watcher.Close()

	lastURL := ""
	lastState := ""
	states := make(chan ipn.State, 8)
	errs := make(chan error, 1)
	go func() {
		for {
			n, err := watcher.Next()
			if err != nil {
				errs <- err
				return
			}
			if n.ErrMessage != nil {
				errs <- errors.New(*n.ErrMessage)
				return
			}
			if n.State != nil {
				states <- *n.State
			}
		}
	}()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	current := ipn.NoState
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-errs:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return errors.New("Tailscale reported a problem: " + err.Error())
		case st := <-states:
			current = st
			if st == ipn.Running {
				return nil
			}
			if st.String() != lastState && st != ipn.NeedsLogin && st != ipn.Starting && st != ipn.NoState {
				lastState = st.String()
				if st == ipn.NeedsMachineAuth {
					// A tailnet that approves new devices by hand: the user
					// approves "ai-hub" on the machines page.
					emit(event{State: "needs-approval", URL: adminMachines})
				} else {
					emit(event{State: "waiting", Detail: st.String()})
				}
			}
		case <-ticker.C:
			if current != ipn.NeedsLogin {
				continue
			}
			st, err := lc.StatusWithoutPeers(ctx)
			if err != nil {
				continue
			}
			if st.AuthURL != "" && st.AuthURL != lastURL {
				lastURL = st.AuthURL
				emit(event{State: "login", URL: st.AuthURL})
			}
		}
	}
}

// waitForHTTPS waits until the tailnet has MagicDNS and HTTPS certificates
// switched on (one-time settings on the tailnet's DNS page) and returns the
// node's DNS name. The tailnet pushes the change, so this polls status only.
func waitForHTTPS(ctx context.Context, lc *local.Client) (string, error) {
	reported := ""
	for {
		st, err := lc.StatusWithoutPeers(ctx)
		if err != nil {
			return "", errors.New("Tailscale status failed: " + err.Error())
		}
		magic := st.CurrentTailnet != nil && st.CurrentTailnet.MagicDNSEnabled
		if magic && len(st.CertDomains) > 0 {
			return strings.TrimSuffix(st.CertDomains[0], "."), nil
		}
		want := "https-disabled"
		if !magic {
			want = "magicdns-disabled"
		}
		if want != reported {
			reported = want
			emit(event{State: want, URL: adminDNS})
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
}

// funnelAllowed says whether this node may use Funnel on 443 right now.
func funnelAllowed(ctx context.Context, lc *local.Client) error {
	st, err := lc.StatusWithoutPeers(ctx)
	if err != nil {
		return err
	}
	if st.Self == nil {
		return errors.New("no node status yet")
	}
	return ipn.CheckFunnelAccess(443, st.Self)
}

// watchFunnel polls until Funnel is allowed, then asks the hub for a restart
// (the hub starts this program again at once on this state).
func watchFunnel(ctx context.Context, lc *local.Client, cancel context.CancelFunc) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Second):
		}
		if funnelAllowed(ctx, lc) == nil {
			emit(event{State: "restarting", Detail: "funnel"})
			cancel()
			return
		}
	}
}

func listen(srv *tsnet.Server, funnel bool) (net.Listener, error) {
	if funnel {
		return srv.ListenFunnel("tcp", ":443")
	}
	return srv.ListenTLS("tcp", ":443")
}

// proxyHandler forwards phone requests to the hub with the marker, after the
// checks the relay path used to do in the hub itself (docs/WORKFLOW-SCOPE.md
// row 6): a request whose Origin is not this address is refused here, and the
// Host and Origin the hub sees are its own loopback ones, so its guards apply
// unchanged. Fetch metadata and cookies travel as the phone sent them.
func proxyHandler(c config, address string) http.Handler {
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(c.upstream)
			pr.Out.Host = c.upstream.Host
			pr.Out.Header.Del(markerHeader)
			pr.Out.Header.Del(bridgeHeader)
			pr.Out.Header.Set(markerHeader, c.marker)
			pr.Out.Header.Set("X-Forwarded-Proto", "https")
			pr.Out.Header.Set("X-Forwarded-Host", strings.TrimPrefix(address, "https://"))
			if pr.In.Header.Get("Origin") != "" {
				pr.Out.Header.Set("Origin", "http://"+c.upstream.Host)
			}
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "The hub did not answer.", "code": "phone-hub-unreachable"})
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" && subtle.ConstantTimeCompare([]byte(origin), []byte(address)) != 1 {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "Untrusted origin"})
			return
		}
		proxy.ServeHTTP(w, r)
	})
}
