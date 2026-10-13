package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
)

//go:embed static/*
var assets embed.FS

type gateway struct {
	token, cookie, user, password, port, guacd, rdpHost, healthURL, instructions string
	active                                                                       atomic.Bool
	context                                                                      context.Context
}

func element(value string) string {
	return strconv.Itoa(utf8.RuneCountInString(value)) + "." + value
}

func instruction(parts ...string) string {
	encoded := make([]string, len(parts))
	for i, part := range parts {
		encoded[i] = element(part)
	}
	return strings.Join(encoded, ",") + ";"
}

func readInstruction(reader *bufio.Reader) ([]string, error) {
	var parts []string
	total := 0
	for len(parts) < 512 {
		var length strings.Builder
		for {
			b, err := reader.ReadByte()
			if err != nil {
				return nil, err
			}
			if b == '.' {
				break
			}
			if b < '0' || b > '9' || length.Len() >= 7 {
				return nil, errors.New("invalid instruction length")
			}
			length.WriteByte(b)
		}
		count, err := strconv.Atoi(length.String())
		if err != nil || count > 1024*1024 {
			return nil, errors.New("invalid instruction length")
		}
		total += count
		if total > 8*1024*1024 {
			return nil, errors.New("instruction too large")
		}
		var value strings.Builder
		for i := 0; i < count; i++ {
			r, size, err := reader.ReadRune()
			if err != nil {
				return nil, err
			}
			if r == utf8.RuneError && size == 1 {
				return nil, errors.New("invalid UTF-8")
			}
			value.WriteRune(r)
		}
		parts = append(parts, value.String())
		separator, err := reader.ReadByte()
		if err != nil {
			return nil, err
		}
		if separator == ';' {
			return parts, nil
		}
		if separator != ',' {
			return nil, errors.New("invalid instruction separator")
		}
	}
	return nil, errors.New("too many instruction elements")
}

func (g *gateway) authenticated(r *http.Request) bool {
	cookie, err := r.Cookie("winboat_web")
	return err == nil && subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(g.cookie)) == 1
}

func (g *gateway) session(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Origin") != "http://"+r.Host {
		http.Error(w, "Invalid origin", 403)
		return
	}
	if !g.authenticated(r) && subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Winboat-Token")), []byte(g.token)) != 1 {
		http.Error(w, "Use the connection URL printed in the web service logs.", 403)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "winboat_web", Value: g.cookie, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, `{"status":"ok"}`)
}

func (g *gateway) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /session", g.session)
	mux.HandleFunc("GET /tunnel", g.tunnel)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"status": "ok", "connected": g.active.Load()})
	})
	static, _ := fs.Sub(assets, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	index := template.Must(template.ParseFS(assets, "static/index.html"))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		index.Execute(w, g.token)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data: blob:; media-src 'self' blob:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'")
		mux.ServeHTTP(w, r)
	})
}

func (g *gateway) waitWindows(ctx context.Context) error {
	client := &http.Client{Timeout: 2 * time.Second}
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, g.healthURL, nil)
		if err != nil {
			return err
		}
		response, err := client.Do(request)
		if err == nil {
			var health struct {
				Status string `json:"status"`
			}
			err = json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&health)
			response.Body.Close()
			if err == nil && response.StatusCode == 200 && health.Status == "ok" {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func dimension(r *http.Request, key, fallback string) string {
	value := r.URL.Query().Get(key)
	if value == "" {
		value = fallback
	}
	number, err := strconv.Atoi(value)
	if err != nil {
		number, _ = strconv.Atoi(fallback)
	}
	return strconv.Itoa(min(max(number, 320), 3840))
}

func (g *gateway) handshake(conn net.Conn, reader *bufio.Reader, r *http.Request) ([]string, error) {
	conn.SetDeadline(time.Now().Add(45 * time.Second))
	if _, err := io.WriteString(conn, instruction("select", "rdp")); err != nil {
		return nil, err
	}
	args, err := readInstruction(reader)
	if err != nil {
		return nil, err
	}
	if args[0] != "args" {
		return nil, errors.New("invalid guacd handshake")
	}
	instructions := fmt.Sprintf(g.instructions, element(dimension(r, "width", "1280")), element(dimension(r, "height", "800")))
	if _, err = io.WriteString(conn, instructions); err != nil {
		return nil, err
	}
	ready, err := readInstruction(reader)
	if err != nil {
		return nil, err
	}
	conn.SetDeadline(time.Time{})
	return ready, nil
}

func (g *gateway) tunnel(w http.ResponseWriter, r *http.Request) {
	ws := g.accept(w, r)
	if ws == nil {
		return
	}
	defer ws.Close()
	if !g.active.CompareAndSwap(false, true) {
		ws.WriteMessage(websocket.TextMessage, []byte(instruction("error", "이미 연결된 브라우저가 있습니다.", "525")))
		return
	}
	defer g.active.Store(false)
	ctx, cancel := context.WithCancel(g.context)
	defer cancel()
	send := sender(ws)
	if send(instruction("", rand.Text())) != nil {
		return
	}
	incoming := make(chan string, 64)
	ws.SetReadLimit(1024 * 1024)
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		defer cancel()
		readBrowser(ctx, ws, send, incoming)
	}()
	defer func() { ws.Close(); <-readDone }()
	stopWS := context.AfterFunc(ctx, func() { ws.Close() })
	defer stopWS()
	conn, reader, failure := g.connectGuacd(ctx, r)
	if failure != "" {
		send(failure)
		return
	}
	relayDone := make(chan struct{})
	go func() {
		defer close(relayDone)
		defer cancel()
		relayToGuacd(ctx, conn, incoming)
	}()
	defer func() { cancel(); conn.Close(); <-relayDone }()
	relayToBrowser(reader, send)
}

func (g *gateway) accept(w http.ResponseWriter, r *http.Request) *websocket.Conn {
	if !g.authenticated(r) || r.Header.Get("Origin") != "http://"+r.Host {
		http.Error(w, "Unauthorized", 403)
		return nil
	}
	supported := false
	for _, protocol := range websocket.Subprotocols(r) {
		if protocol == "guacamole" {
			supported = true
		}
	}
	if !supported {
		http.Error(w, "Guacamole subprotocol required", 400)
		return nil
	}
	upgrader := websocket.Upgrader{Subprotocols: []string{"guacamole"}, CheckOrigin: func(r *http.Request) bool { return r.Header.Get("Origin") == "http://"+r.Host }}
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return nil
	}
	return ws
}

func sender(ws *websocket.Conn) func(string) error {
	var writeMutex sync.Mutex
	return func(data string) error {
		writeMutex.Lock()
		defer writeMutex.Unlock()
		ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
		return ws.WriteMessage(websocket.TextMessage, []byte(data))
	}
}

func readBrowser(ctx context.Context, ws *websocket.Conn, send func(string) error, incoming chan<- string) {
	for {
		kind, data, err := ws.ReadMessage()
		if err != nil {
			return
		}
		if kind != websocket.TextMessage {
			return
		}
		reader := bufio.NewReader(strings.NewReader(string(data)))
		for {
			parts, err := readInstruction(reader)
			if err == io.EOF {
				break
			}
			if err != nil {
				return
			}
			if parts[0] == "" {
				if len(parts) == 3 && parts[1] == "ping" {
					if send(instruction(parts...)) != nil {
						return
					}
				}
			} else {
				select {
				case incoming <- instruction(parts...):
				case <-ctx.Done():
					return
				}
			}
		}
	}
}

// connectGuacd returns the error instruction to send to the browser when it fails.
func (g *gateway) connectGuacd(ctx context.Context, r *http.Request) (net.Conn, *bufio.Reader, string) {
	boot, stopBoot := context.WithTimeout(ctx, 180*time.Second)
	err := g.waitWindows(boot)
	stopBoot()
	if err != nil {
		slog.Error("Windows health check failed", "url", g.healthURL, "error", err)
		return nil, nil, instruction("error", "Windows가 준비되지 않았습니다.", "512")
	}
	conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", g.guacd)
	if err != nil {
		slog.Error("guacd connection failed", "addr", g.guacd, "error", err)
		return nil, nil, instruction("error", "guacd에 연결하지 못했습니다.", "512")
	}
	context.AfterFunc(ctx, func() { conn.Close() })
	reader := bufio.NewReader(conn)
	ready, err := g.handshake(conn, reader, r)
	if err != nil {
		conn.Close()
		slog.Error("guacd handshake failed", "error", err)
		return nil, nil, instruction("error", "Windows RDP 연결에 실패했습니다.", "512")
	}
	if ready[0] == "error" {
		conn.Close()
		slog.Error("guacd returned error", "args", ready[1:])
		return nil, nil, instruction(ready...)
	}
	if ready[0] != "ready" {
		conn.Close()
		slog.Error("unexpected guacd instruction", "opcode", ready[0])
		return nil, nil, instruction("error", "guacd handshake failed", "512")
	}
	return conn, reader, ""
}

func relayToGuacd(ctx context.Context, conn net.Conn, incoming <-chan string) {
	for {
		select {
		case data := <-incoming:
			conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if _, err := io.WriteString(conn, data); err != nil {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

func relayToBrowser(reader *bufio.Reader, send func(string) error) {
	for {
		parts, err := readInstruction(reader)
		if err != nil {
			return
		}
		if send(instruction(parts...)) != nil {
			return
		}
	}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	g := &gateway{
		token:        rand.Text(),
		cookie:       rand.Text(),
		context:      ctx,
		user:         os.Getenv("WINBOAT_USERNAME"),
		password:     os.Getenv("WINBOAT_PASSWORD"),
		port:         os.Getenv("WEB_PORT"),
		guacd:        os.Getenv("GUACD_ADDR"),
		rdpHost:      os.Getenv("RDP_HOST"),
		healthURL:    os.Getenv("WINBOAT_HEALTH_URL"),
		instructions: os.Getenv("GUACD_INSTRUCTIONS"),
	}

	if g.user == "" || g.password == "" || g.port == "" || g.guacd == "" || g.instructions == "" {
		slog.Error("required environment missing", "required", "WINBOAT_USERNAME, WINBOAT_PASSWORD, WEB_PORT, GUACD_ADDR, GUACD_INSTRUCTIONS")
		os.Exit(1)
	}
	server := &http.Server{Addr: ":8000", Handler: g.handler(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		slog.Error("listen failed", "addr", server.Addr, "error", err)
		os.Exit(1)
	}

	fmt.Printf("접속 주소: http://127.0.0.1:%s/\n", g.port)
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()

	select {
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(shutdown)
	case err := <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server stopped", "error", err)
			os.Exit(1)
		}
	}
}
