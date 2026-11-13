// Command liff-e2e serves the real LIFF page (liff.Page) next to a fake /api/v1/me API for the browser tests in tests/e2e/liff.
// It binds to loopback only and prints "LISTEN <port>" as its first stdout line once it accepts connections.
package main

import (
	"chatbot-go/internal/httputil"
	"chatbot-go/internal/liff"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/labstack/echo/v4"
)

const (
	// liffID is the public sample ID the page tests already use.
	liffID = "2001234567-AbCdEfGh"
	// fakeIDValue is what the fake LIFF SDK returns; the API only accepts this bearer value.
	fakeIDValue = "e2e-fake-id-token"
	// maxBodyBytes caps every request body the fake API reads.
	maxBodyBytes = 1 << 20
)

type api struct {
	st *state
}

// newServer mounts the real page and static handlers (same paths as the product router) and the fake API.
func newServer(goldenDir string) (*echo.Echo, error) {
	g, err := loadGolden(goldenDir)
	if err != nil {
		return nil, err
	}
	st, err := newState(g)
	if err != nil {
		return nil, err
	}
	page, static, err := liff.Page(liffID, liff.WithAIProviders([]string{"gemini", "groq"}))
	if err != nil {
		return nil, fmt.Errorf("build page: %w", err)
	}
	e := echo.New()
	e.HideBanner = true
	e.HidePort = true
	e.GET("/liff", page)
	e.GET("/liff/", page)
	e.GET("/liff/static/*", static)
	a := &api{st: st}
	e.Any("/api/v1/me/*", a.serve)
	e.POST("/__e2e/reset", a.reset)
	e.POST("/__e2e/fail", a.fail)
	e.POST("/__e2e/delay", a.delay)
	e.POST("/__e2e/clear", a.clear)
	e.POST("/__e2e/release", a.release)
	e.POST("/__e2e/alerts", a.alerts)
	e.GET("/__e2e/state", a.state)
	return e, nil
}

// serve answers one fake API call: log it, then apply any failure and delay rule that matches.
func (a *api) serve(c echo.Context) error {
	r := c.Request()
	rel := strings.TrimPrefix(r.URL.Path, "/api/v1/me")
	m := a.st.begin(r.Method, rel, r.URL.RawQuery)
	if r.Header.Get("Authorization") != "Bearer "+fakeIDValue {
		a.st.finish(m.index, http.StatusUnauthorized)
		return writeReply(c, errReply(http.StatusUnauthorized, httputil.ErrorCodeInvalidSignature, "unauthorized"))
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		a.st.finish(m.index, http.StatusBadRequest)
		return writeReply(c, badRequest("unreadable body"))
	}
	req := &request{method: r.Method, path: rel, query: r.URL.RawQuery, body: body, arrival: m.seq}
	var rep reply
	switch {
	case m.delay == nil:
		rep = a.st.compute(req, m.fail)
	case m.delay.Late:
		a.st.hold(r.Context(), m.delay.MS)
		rep = a.st.compute(req, m.fail)
	default:
		rep = a.st.compute(req, m.fail)
		a.st.hold(r.Context(), m.delay.MS)
	}
	a.st.finish(m.index, rep.status)
	return writeReply(c, rep)
}

func writeReply(c echo.Context, rep reply) error {
	c.Response().Header().Set("Cache-Control", "no-store")
	if rep.status == http.StatusNoContent {
		return c.NoContent(rep.status)
	}
	return c.Blob(rep.status, echo.MIMEApplicationJSON, rep.body)
}

// decode reads a control request body (size capped, unknown fields rejected).
func decode(c echo.Context, v any) error {
	dec := json.NewDecoder(io.LimitReader(c.Request().Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid control body")
	}
	return nil
}

func (a *api) reset(c echo.Context) error {
	a.st.reset()
	return c.NoContent(http.StatusNoContent)
}

func (a *api) fail(c echo.Context) error {
	var f failRule
	if err := decode(c, &f); err != nil {
		return err
	}
	if f.Status < http.StatusBadRequest || f.Status > 599 {
		return echo.NewHTTPError(http.StatusBadRequest, "status must be an HTTP error status")
	}
	a.st.addFail(f)
	return c.NoContent(http.StatusNoContent)
}

func (a *api) delay(c echo.Context) error {
	var d delayRule
	if err := decode(c, &d); err != nil {
		return err
	}
	if d.MS <= 0 || d.MS > 60000 {
		return echo.NewHTTPError(http.StatusBadRequest, "ms must be 1..60000")
	}
	a.st.addDelay(d)
	return c.NoContent(http.StatusNoContent)
}

func (a *api) clear(c echo.Context) error {
	a.st.clearRules()
	return c.NoContent(http.StatusNoContent)
}

func (a *api) release(c echo.Context) error {
	a.st.release()
	return c.NoContent(http.StatusNoContent)
}

func (a *api) alerts(c echo.Context) error {
	var in struct {
		Quake   bool     `json:"quake"`
		Weather []string `json:"weather"`
	}
	if err := decode(c, &in); err != nil {
		return err
	}
	a.st.setAlerts(in.Quake, in.Weather)
	return c.NoContent(http.StatusNoContent)
}

func (a *api) state(c echo.Context) error {
	return c.JSON(http.StatusOK, a.st.snapshot())
}

func run(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("liff-e2e", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:0", "listen address (loopback only)")
	goldenDir := fs.String("golden-dir", "", "directory holding the shared golden JSON files")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *goldenDir == "" {
		return errors.New("-golden-dir is required")
	}
	host, _, err := net.SplitHostPort(*addr)
	if err != nil {
		return fmt.Errorf("bad -addr: %w", err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("-addr must be a loopback IP, got %q", host)
	}
	e, err := newServer(*goldenDir)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", *addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	srv := &http.Server{Handler: e, ReadHeaderTimeout: 5 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	if _, err := fmt.Fprintf(stdout, "LISTEN %d\n", ln.Addr().(*net.TCPAddr).Port); err != nil {
		return err
	}
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "liff-e2e:", err)
		os.Exit(1)
	}
}
