package main

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
	"go.uber.org/zap"
	"golang.org/x/time/rate"
)

type config struct {
	UpstreamURL string
	ListenAddr  string
	RateLimit   float64
	RateBurst   int
}

func loadConfig() config {
	// .env не обязателен — если его нет, читаем из окружения как есть
	_ = godotenv.Load()

	return config{
		UpstreamURL: getEnv("UPSTREAM_URL", "https://api.green-api.com"),
		ListenAddr:  getEnv("LISTEN_ADDR", ":8080"),
		RateLimit:   getEnvFloat("RATE_LIMIT", 5),
		RateBurst:   getEnvInt("RATE_BURST", 10),
	}
}

func getEnv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func getEnvFloat(key string, def float64) float64 {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func getEnvInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return def
}

func main() {
	logger, err := zap.NewProduction()
	if err != nil {
		panic(err)
	}
	defer logger.Sync()

	cfg := loadConfig()

	target, err := url.Parse(cfg.UpstreamURL)
	if err != nil {
		logger.Fatal("invalid upstream url", zap.Error(err))
	}

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.URL.Path = strings.TrimPrefix(pr.In.URL.Path, "/api")
			pr.Out.URL.RawPath = strings.TrimPrefix(pr.In.URL.RawPath, "/api")
			pr.Out.Host = target.Host
			pr.SetXForwarded()
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			logger.Error("proxy error",
				zap.String("path", r.URL.Path),
				zap.Error(err),
			)
			http.Error(w, "bad gateway", http.StatusBadGateway)
		},
	}

	limiter := rate.NewLimiter(rate.Limit(cfg.RateLimit), cfg.RateBurst)

	mux := http.NewServeMux()
	mux.Handle("/api/", rateLimitMiddleware(limiter, logger, proxy))
	mux.Handle("/", http.FileServer(http.Dir("./static")))

	handler := loggingMiddleware(logger, mux)

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	logger.Info("server started",
		zap.String("addr", cfg.ListenAddr),
		zap.String("upstream", cfg.UpstreamURL),
		zap.Float64("rate_limit", cfg.RateLimit),
		zap.Int("rate_burst", cfg.RateBurst),
	)
	if err := srv.ListenAndServe(); err != nil {
		logger.Fatal("server stopped", zap.Error(err))
	}
}

// statusRecorder перехватывает код ответа для логирования.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.status = code
	sr.ResponseWriter.WriteHeader(code)
}

func loggingMiddleware(logger *zap.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sr := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sr, r)
		logger.Info("request",
			zap.String("method", r.Method),
			zap.String("path", r.URL.Path),
			zap.Int("status", sr.status),
			zap.Duration("duration", time.Since(start)),
			zap.String("remote", r.RemoteAddr),
		)
	})
}

func rateLimitMiddleware(limiter *rate.Limiter, logger *zap.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !limiter.Allow() {
			logger.Warn("rate limit exceeded",
				zap.String("path", r.URL.Path),
				zap.String("remote", r.RemoteAddr),
			)
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}
