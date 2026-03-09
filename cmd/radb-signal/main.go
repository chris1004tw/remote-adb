package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	signalpkg "github.com/chris1004tw/remote-adb/internal/signal"
)

const version = "0.1.0-dev"

func main() {
	port := flag.Int("port", envInt("RADB_SIGNAL_PORT", 8080), "HTTP/WebSocket 監聽埠")
	host := flag.String("host", envStr("RADB_SIGNAL_HOST", "0.0.0.0"), "監聯地址")
	token := flag.String("token", envStr("RADB_TOKEN", ""), "PSK 驗證 Token")
	flag.Parse()

	if *token == "" {
		fmt.Fprintln(os.Stderr, "錯誤：必須設定 RADB_TOKEN 環境變數或使用 --token flag")
		os.Exit(1)
	}

	slog.Info("啟動 radb-signal", "version", version, "host", *host, "port", *port)

	hub := signalpkg.NewHub()
	auth := signalpkg.NewPSKAuth(*token)
	srv := signalpkg.NewServer(hub, auth)

	httpServer := &http.Server{
		Addr:    fmt.Sprintf("%s:%d", *host, *port),
		Handler: srv.Handler(),
	}

	// 優雅關閉
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	go func() {
		slog.Info("Signal Server 開始監聽", "addr", httpServer.Addr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("HTTP server 錯誤", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	slog.Info("收到關閉信號，準備優雅關閉...")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		slog.Error("HTTP server 關閉失敗", "error", err)
	}
	slog.Info("Signal Server 已關閉")
}

func envStr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	return fallback
}
