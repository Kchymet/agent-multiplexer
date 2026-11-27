package direct

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"
)

// ServeConfig is `amux device serve`.
type ServeConfig struct {
	Root            string
	Listen          string // ip:port
	AllowPublicBind bool
	CertFile        string
	KeyFile         string
	Endpoints       []string
	AllowOrigins    []string
	Life            Lifetimes
	Backend         Backend
	Verify          HostVerifier
	Logf            func(string, ...any)
	// GetCertificate, when set, supplies the serving certificate for each new
	// handshake instead of CertFile/KeyFile, so a renewed certificate can be
	// swapped in while existing connections keep theirs. HTTPS stays
	// mandatory: there is no way to serve without a certificate.
	GetCertificate func(*tls.ClientHelloInfo) (*tls.Certificate, error)
	// ExtraOrigins, when set, supplies further exact browser origins at
	// request time (Config.ExtraOrigins).
	ExtraOrigins func() []string
	// Ready, when set, is called once the listener and admin socket are up.
	Ready func(addr net.Addr, serverID string)
	// ListenCheck is how often the listener checks that its address is
	// still assigned to this host (default 30s; ConnStats.Listen).
	ListenCheck time.Duration
}

// Serve runs the direct server until ctx ends. Every check that can refuse the
// configuration runs before the listener binds.
func Serve(ctx context.Context, cfg ServeConfig) error {
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if err := CheckBind(cfg.Listen, cfg.AllowPublicBind); err != nil {
		return err
	}
	// HTTP/2 is offered first. Over HTTP/1.1 a browser opens at most six
	// connections per origin, shared by every tab, and each open transcript
	// stream (SSE) holds one for as long as it is open: with a few sessions
	// open, every further request — a token refresh, a hello, a history page —
	// queues behind them indefinitely and the backend looks unreachable.
	// HTTP/2 carries all of them on one connection. The watch WebSocket stays
	// HTTP/1.1: the server does not advertise extended CONNECT (RFC 8441), so
	// clients open it on a connection of its own, as before.
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12, NextProtos: []string{"h2", "http/1.1"}}
	if cfg.GetCertificate != nil {
		c, err := cfg.GetCertificate(nil)
		if err != nil || c == nil {
			return fmt.Errorf("load TLS certificate: %v", err)
		}
		tlsCfg.GetCertificate = cfg.GetCertificate
	} else {
		if cfg.CertFile == "" || cfg.KeyFile == "" {
			return errors.New("direct serve requires --tls-cert and --tls-key (HTTPS is mandatory, even on a private network)")
		}
		cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			return fmt.Errorf("load TLS certificate: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}
	if cfg.Verify == nil {
		return errors.New("direct serve needs a host verifier for its admin channel")
	}
	a, err := OpenAuthority(cfg.Root, cfg.Life, nil)
	if errors.Is(err, ErrStoreBusy) {
		return errors.New("another `amux device serve` already owns the direct store")
	}
	if err != nil {
		return err
	}
	defer a.Close()
	wake := NewWakeService(a, cfg.Root, cfg.Backend, cfg.Logf)
	approvals := NewApprovalQueue()
	srv, err := NewServer(Config{Authority: a, Backend: cfg.Backend, Endpoints: cfg.Endpoints, AllowOrigins: cfg.AllowOrigins, Logf: cfg.Logf, Wake: wake,
		Approvals: approvals, ExtraOrigins: cfg.ExtraOrigins})
	if err != nil {
		return err
	}
	admin := &AdminServer{Authority: a, Endpoints: srv.cfg.Endpoints, Verify: cfg.Verify, Logf: cfg.Logf, Wake: wake, Approvals: approvals,
		Conns: srv.ConnStats}
	if err := admin.Listen(cfg.Root); err != nil {
		return fmt.Errorf("admin socket: %w", err)
	}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		admin.ln.Close()
		return err
	}
	// shutdown fires when the caller's ctx ends; the serve context is
	// cancelled only after the server is marked closing, so streams it ends
	// report server_closed.
	shutdown := ctx.Done()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tlsLn := tls.NewListener(ln, tlsCfg)
	hs := &http.Server{
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    16 << 10,
		// The request context is the serve context, so shutting down ends
		// every stream at once rather than after Shutdown's grace.
		BaseContext: func(net.Listener) context.Context { return ctx },
		ConnState:   srv.tel.connState,
		ErrorLog:    log.New(errorLog{srv.tel}, "", 0),
	}
	go wake.Run(ctx)
	adminErr := make(chan error, 1)
	go func() { adminErr <- admin.Serve(ctx) }()
	go func() {
		select {
		case <-shutdown:
		case <-ctx.Done(): // Serve failed on its own
			return
		}
		srv.beginShutdown()
		cancel()
		shut, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		_ = hs.Shutdown(shut)
	}()
	interval := cfg.ListenCheck
	if interval <= 0 {
		interval = 30 * time.Second
	}
	go srv.tel.watchListen(ctx, ln.Addr().String(), interval, net.InterfaceAddrs)
	if cfg.Ready != nil {
		cfg.Ready(ln.Addr(), a.ServerID())
	}
	err = hs.Serve(tlsLn)
	cancel()
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	if aerr := <-adminErr; err == nil && aerr != nil {
		err = aerr
	}
	return err
}
