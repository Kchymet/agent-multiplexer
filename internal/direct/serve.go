package direct

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

// ServeConfig is `amux direct serve`.
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
	// Ready, when set, is called once the listener and admin socket are up.
	Ready func(addr net.Addr, serverID string)
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
	if cfg.CertFile == "" || cfg.KeyFile == "" {
		return errors.New("direct serve requires --tls-cert and --tls-key (HTTPS is mandatory, even on a private network)")
	}
	cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return fmt.Errorf("load TLS certificate: %w", err)
	}
	if cfg.Verify == nil {
		return errors.New("direct serve needs a host verifier for its admin channel")
	}
	a, err := OpenAuthority(cfg.Root, cfg.Life, nil)
	if errors.Is(err, ErrStoreBusy) {
		return errors.New("another `amux direct serve` already owns the direct store")
	}
	if err != nil {
		return err
	}
	defer a.Close()
	srv, err := NewServer(Config{Authority: a, Backend: cfg.Backend, Endpoints: cfg.Endpoints, AllowOrigins: cfg.AllowOrigins, Logf: cfg.Logf})
	if err != nil {
		return err
	}
	admin := &AdminServer{Authority: a, Endpoints: srv.cfg.Endpoints, Verify: cfg.Verify, Logf: cfg.Logf}
	if err := admin.Listen(cfg.Root); err != nil {
		return fmt.Errorf("admin socket: %w", err)
	}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		admin.ln.Close()
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	tlsLn := tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	hs := &http.Server{
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    16 << 10,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	adminErr := make(chan error, 1)
	go func() { adminErr <- admin.Serve(ctx) }()
	go func() {
		<-ctx.Done()
		shut, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		_ = hs.Shutdown(shut)
	}()
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
