package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/DataDog/datadog-go/statsd"
	"github.com/go-redis/redis/v8"
	"github.com/gofrs/uuid"
	"github.com/gorilla/mux"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sideshow/apns2/token"
	"go.opentelemetry.io/otel"
	"go.uber.org/zap"

	"github.com/christianselig/apollo-backend/internal/domain"
	"github.com/christianselig/apollo-backend/internal/push"
	"github.com/christianselig/apollo-backend/internal/reddit"
	"github.com/christianselig/apollo-backend/internal/repository"
)

const (
	registrationSecretEnv          = "REGISTRATION_SECRET"
	unsafeDevelopmentAuthBypassEnv = "APOLLO_UNSAFE_ALLOW_MISSING_REGISTRATION_SECRET"
	minRegistrationSecretBytes     = 32
	maxRequestBodyBytes            = 1 << 20 // 1 MiB
	maxRequestHeaderBytes          = 16 << 10
)

type redditIdentityVerifier interface {
	MeWithAccessToken(context.Context, string, ...reddit.RequestOption) (*reddit.MeResponse, error)
}

type api struct {
	logger         *zap.Logger
	statsd         statsd.ClientInterface
	reddit         *reddit.Client
	redditIdentity redditIdentityVerifier
	apns           *token.Token
	apnsTopic      string
	httpClient     *http.Client
	sender         *push.Sender

	accountRepo      domain.AccountRepository
	deviceRepo       domain.DeviceRepository
	subredditRepo    domain.SubredditRepository
	watcherRepo      domain.WatcherRepository
	userRepo         domain.UserRepository
	liveActivityRepo domain.LiveActivityRepository
}

func NewAPI(ctx context.Context, logger *zap.Logger, statsd statsd.ClientInterface, redis *redis.Client, pool *pgxpool.Pool, apns *token.Token, apnsTopic string) *api {
	tracer := otel.Tracer("api")

	reddit := reddit.NewClient(
		tracer,
		statsd,
		redis,
		16,
	)

	accountRepo := repository.NewPostgresAccount(pool)
	deviceRepo := repository.NewPostgresDevice(pool)
	subredditRepo := repository.NewPostgresSubreddit(pool)
	watcherRepo := repository.NewPostgresWatcher(pool)
	userRepo := repository.NewPostgresUser(pool)
	liveActivityRepo := repository.NewPostgresLiveActivity(pool)

	client := &http.Client{}

	return &api{
		logger:         logger,
		statsd:         statsd,
		reddit:         reddit,
		redditIdentity: reddit,
		apns:           apns,
		apnsTopic:      apnsTopic,
		httpClient:     client,
		sender:         push.NewSender(logger, apns, apnsTopic),

		accountRepo:      accountRepo,
		deviceRepo:       deviceRepo,
		subredditRepo:    subredditRepo,
		watcherRepo:      watcherRepo,
		userRepo:         userRepo,
		liveActivityRepo: liveActivityRepo,
	}
}

func (a *api) Server(port int) *http.Server {
	addr := fmt.Sprintf(":%d", port)
	if os.Getenv(registrationSecretEnv) == "" && unsafeDevelopmentAuthBypassEnabled() {
		addr = fmt.Sprintf("127.0.0.1:%d", port)
	}
	return &http.Server{
		Addr:              addr,
		Handler:           a.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    maxRequestHeaderBytes,
	}
}

func (a *api) Routes() *mux.Router {
	r := mux.NewRouter()
	// Prevent mux from issuing an unauthenticated path-cleaning redirect before
	// the authentication middleware runs. Non-canonical paths fall through to
	// the authenticated catch-all below instead.
	r.SkipClean(true)

	r.HandleFunc("/v1/health", a.healthCheckHandler).Methods("GET")

	r.HandleFunc("/v1/device", a.upsertDeviceHandler).Methods("POST")
	r.HandleFunc("/v1/device/{apns}/account", a.upsertAccountHandler).Methods("POST")
	r.HandleFunc("/v1/device/{apns}/accounts", a.upsertAccountsHandler).Methods("POST")
	r.HandleFunc("/v1/live_activities", a.createLiveActivityHandler).Methods("POST")

	r.HandleFunc("/v1/device/{apns}", a.deleteDeviceHandler).Methods("DELETE")
	r.HandleFunc("/v1/device/{apns}/test", a.testDeviceHandler).Methods("POST")
	r.HandleFunc("/v1/device/{apns}/test/comment_reply", generateNotificationTester(a, commentReply)).Methods("POST")
	r.HandleFunc("/v1/device/{apns}/test/post_reply", generateNotificationTester(a, postReply)).Methods("POST")
	r.HandleFunc("/v1/device/{apns}/test/private_message", generateNotificationTester(a, privateMessage)).Methods("POST")
	r.HandleFunc("/v1/device/{apns}/test/subreddit_watcher", generateNotificationTester(a, subredditWatcher)).Methods("POST")
	r.HandleFunc("/v1/device/{apns}/test/trending_post", generateNotificationTester(a, trendingPost)).Methods("POST")
	r.HandleFunc("/v1/device/{apns}/test/username_mention", generateNotificationTester(a, usernameMention)).Methods("POST")

	r.HandleFunc("/v1/device/{apns}/account/{redditID}", a.disassociateAccountHandler).Methods("DELETE")
	r.HandleFunc("/v1/device/{apns}/account/{redditID}/notifications", a.notificationsAccountHandler).Methods("PATCH")
	r.HandleFunc("/v1/device/{apns}/account/{redditID}/notifications", a.getNotificationsAccountHandler).Methods("GET")

	r.HandleFunc("/v1/device/{apns}/account/{redditID}/watcher", a.createWatcherHandler).Methods("POST")
	r.HandleFunc("/v1/device/{apns}/account/{redditID}/watcher/{watcherID}", a.deleteWatcherHandler).Methods("DELETE")
	r.HandleFunc("/v1/device/{apns}/account/{redditID}/watcher/{watcherID}", a.editWatcherHandler).Methods("PATCH")
	r.HandleFunc("/v1/device/{apns}/account/{redditID}/watchers", a.listWatchersHandler).Methods("GET")

	// Diagnostic stubs for endpoints Apollo iOS posts to under its three
	// legacy hosts (apollopushserver.xyz, beta.apollonotifications.com,
	// apolloreq.com) that the tweak rewrites here. Their exact request /
	// response shapes are not public; we discard the body and return a
	// permissive empty 200 so the client doesn't treat the call as a
	// failure. See [[onboarding-receipt-bypass]].
	r.HandleFunc("/api/req_v2", a.reqV2Handler).Methods("POST")
	r.HandleFunc("/api/announcement", a.announcementHandler).Methods("GET")
	r.HandleFunc("/v1/receipt", a.checkReceiptHandler).Methods("POST")
	r.HandleFunc("/v1/receipt/{apns}", a.checkReceiptHandler).Methods("POST")
	// Gorilla mux does not apply router middleware to its NotFound and
	// MethodNotAllowed handlers. A final catch-all keeps those requests inside
	// the authentication and body-limit chain as well.
	r.PathPrefix("/").HandlerFunc(a.notFoundLogger)

	// Keep the health endpoint available to the container health check. Every
	// other route, including diagnostics and the NotFound handler, requires the
	// registration token before its body is read or a handler runs.
	r.Use(a.loggingMiddleware)
	r.Use(a.requestIdMiddleware)
	r.Use(a.registrationAuthMiddleware)
	r.Use(a.bodyLimitMiddleware)

	return r
}

// ValidateConfiguration prevents a public deployment from starting without
// authentication. The bypass is intentionally both development-only and
// conspicuous so it cannot silently make a production deployment public.
func ValidateConfiguration() error {
	secret := os.Getenv(registrationSecretEnv)
	if validRegistrationSecret(secret) {
		return nil
	}
	if secret == "" && unsafeDevelopmentAuthBypassEnabled() {
		return nil
	}
	if secret != "" {
		return fmt.Errorf("%s must contain at least %d bytes", registrationSecretEnv, minRegistrationSecretBytes)
	}
	return fmt.Errorf("%s must be set; only set ENV=development and %s=1 for unsafe local development", registrationSecretEnv, unsafeDevelopmentAuthBypassEnv)
}

func validRegistrationSecret(secret string) bool {
	return len(secret) >= minRegistrationSecretBytes
}

func unsafeDevelopmentAuthBypassEnabled() bool {
	return os.Getenv("ENV") == "development" && os.Getenv(unsafeDevelopmentAuthBypassEnv) == "1"
}

func isHealthRequest(r *http.Request) bool {
	return r.Method == http.MethodGet && r.URL.Path == "/v1/health"
}

// registrationAuthMiddleware authenticates every non-health request. It is a
// deployment-wide shared secret until clients support a server-issued,
// per-device management credential.
func (a *api) registrationAuthMiddleware(next http.Handler) http.Handler {
	secret := []byte(os.Getenv(registrationSecretEnv))
	secretDigest := sha256.Sum256(secret)
	secretValid := validRegistrationSecret(string(secret))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isHealthRequest(r) {
			next.ServeHTTP(w, r)
			return
		}
		if !secretValid {
			if len(secret) == 0 && unsafeDevelopmentAuthBypassEnabled() {
				next.ServeHTTP(w, r)
				return
			}
			http.Error(w, "server authentication is not configured", http.StatusServiceUnavailable)
			return
		}

		providedDigest := sha256.Sum256([]byte(r.Header.Get("X-Registration-Token")))
		if subtle.ConstantTimeCompare(providedDigest[:], secretDigest[:]) != 1 {
			http.Error(w, "missing or invalid registration token", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// bodyLimitMiddleware buffers at most one MiB before dispatch. The API only
// accepts small JSON/diagnostic payloads, so buffering gives every handler the
// same strict limit, including handlers that otherwise discard their body.
func (a *api) bodyLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body == nil {
			next.ServeHTTP(w, r)
			return
		}
		if r.ContentLength > maxRequestBodyBytes {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}

		limited := http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
		body, err := io.ReadAll(limited)
		_ = r.Body.Close()
		if err != nil {
			var maxBytesErr *http.MaxBytesError
			if errors.As(err, &maxBytesErr) {
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			} else {
				http.Error(w, "failed to read request body", http.StatusBadRequest)
			}
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		next.ServeHTTP(w, r)
	})
}

type LoggingResponseWriter struct {
	w          http.ResponseWriter
	statusCode int
	bytes      int
}

func (lrw *LoggingResponseWriter) Header() http.Header {
	return lrw.w.Header()
}

func (lrw *LoggingResponseWriter) Write(bb []byte) (int, error) {
	if lrw.statusCode == 0 {
		lrw.statusCode = http.StatusOK
	}
	wb, err := lrw.w.Write(bb)
	lrw.bytes += wb
	return wb, err
}

func (lrw *LoggingResponseWriter) WriteHeader(statusCode int) {
	lrw.w.WriteHeader(statusCode)
	lrw.statusCode = statusCode
}

func (a *api) requestIdMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := uuid.Must(uuid.NewV4()).String()
		w.Header().Set("X-Apollo-Request-Id", id)
		next.ServeHTTP(w, r)
	})
}

func (a *api) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip logging health checks
		if isHealthRequest(r) {
			next.ServeHTTP(w, r)
			return
		}

		start := time.Now()
		lrw := &LoggingResponseWriter{w: w}

		// Call the next handler, which can be another middleware in the chain, or the final handler.
		next.ServeHTTP(lrw, r)

		duration := time.Since(start).Milliseconds()

		peerAddr := "unknown"
		if ip, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
			peerAddr = ip
		}

		fields := []zap.Field{
			zap.Int64("duration", duration),
			zap.String("method", r.Method),
			// Never trust X-Forwarded-For without a configured trusted-proxy
			// boundary. This is the directly connected peer, not a claimed client.
			zap.String("peer#addr", peerAddr),
			zap.Int("response#bytes", lrw.bytes),
			zap.Int("status", lrw.statusCode),
			zap.String("route", requestRouteTemplate(r)),
			zap.String("request#id", lrw.Header().Get("X-Apollo-Request-Id")),
		}

		if lrw.statusCode == 200 {
			a.logger.Info("", fields...)
		} else {
			err := lrw.Header().Get("X-Apollo-Error")
			a.logger.Error(err, fields...)
		}

		tags := []string{fmt.Sprintf("status:%d", lrw.statusCode)}
		_ = a.statsd.Histogram("api.latency", float64(duration), nil, 1.0)
		_ = a.statsd.Incr("api.calls", tags, 1.0)
		if lrw.statusCode >= 500 {
			_ = a.statsd.Incr("api.errors", nil, 1.0)
		}
	})
}

func requestRouteTemplate(r *http.Request) string {
	route := mux.CurrentRoute(r)
	if route == nil {
		return "unmatched"
	}
	template, err := route.GetPathTemplate()
	if err != nil || template == "" {
		return "unmatched"
	}
	return template
}
