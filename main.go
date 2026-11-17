package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func main() {
	os.Exit(run())
}

func run() int {
	configPath := flag.String("config", defaultConfigPath(), "配置文件路径")
	flag.Parse()

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "feishu-token-proxy:", err)
		return 1
	}

	var out io.Writer = os.Stdout
	if cfg.LogFile != "" {
		f, err := os.OpenFile(cfg.LogFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			fmt.Fprintln(os.Stderr, "feishu-token-proxy: 打开日志文件失败：", err)
			return 1
		}
		defer f.Close()
		out = teeWriter{os.Stdout, f}
	}
	logger := log.New(out, "", log.LstdFlags)

	transport := NewUpstreamTransport()
	tokens := NewTokenManager(cfg.UpstreamURL(), cfg.AppID, cfg.AppSecret, cfg.LazyRefresh,
		&http.Client{Transport: transport, Timeout: 15 * time.Second}, logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 先占端口再取 token，端口被占用时立即失败
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		logger.Printf("启动失败：%v", err)
		return 1
	}
	startCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	_, _, err = tokens.Token(startCtx)
	cancel()
	if err != nil {
		ln.Close()
		logger.Printf("启动失败：%v", err)
		return 1
	}

	mode := "后台定时刷新 token"
	if cfg.LazyRefresh {
		mode = "按需刷新 token（lazy_refresh）"
	} else {
		go tokens.Run(ctx)
	}
	srv := &http.Server{
		Handler:           NewServer(cfg.UpstreamURL(), tokens, transport, logger).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	logger.Printf("已启动，监听 http://%s，上游 %s，%s", cfg.Listen, cfg.Upstream, mode)

	select {
	case err := <-serveErr:
		logger.Printf("服务异常退出：%v", err)
		return 1
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Printf("服务异常退出：%v", err)
		return 1
	}
	logger.Printf("已退出")
	return 0
}

// teeWriter 把每次写入依次交给所有 writer，某个 writer 出错时其余照常写入。
// 无控制台启动（如任务计划程序）时 stdout 句柄无效，日志文件仍能写入。
type teeWriter []io.Writer

func (t teeWriter) Write(p []byte) (int, error) {
	for _, w := range t {
		_, _ = w.Write(p)
	}
	return len(p), nil
}

// defaultConfigPath 返回 exe 所在目录下的 config.json。
func defaultConfigPath() string {
	exe, err := os.Executable()
	if err != nil {
		return "config.json"
	}
	return filepath.Join(filepath.Dir(exe), "config.json")
}
