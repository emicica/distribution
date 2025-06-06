package handlers

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"expvar"
	"fmt"
	"math"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/distribution/distribution/v3"
	"github.com/distribution/distribution/v3/configuration"
	"github.com/distribution/distribution/v3/health"
	"github.com/distribution/distribution/v3/health/checks"
	"github.com/distribution/distribution/v3/internal/dcontext"
	prometheus "github.com/distribution/distribution/v3/metrics"
	"github.com/distribution/distribution/v3/notifications"
	"github.com/distribution/distribution/v3/registry/api/errcode"
	v2 "github.com/distribution/distribution/v3/registry/api/v2"
	"github.com/distribution/distribution/v3/registry/auth"
	registrymiddleware "github.com/distribution/distribution/v3/registry/middleware/registry"
	repositorymiddleware "github.com/distribution/distribution/v3/registry/middleware/repository"
	"github.com/distribution/distribution/v3/registry/proxy"
	"github.com/distribution/distribution/v3/registry/storage"
	memorycache "github.com/distribution/distribution/v3/registry/storage/cache/memory"
	rediscache "github.com/distribution/distribution/v3/registry/storage/cache/redis"
	storagedriver "github.com/distribution/distribution/v3/registry/storage/driver"
	"github.com/distribution/distribution/v3/registry/storage/driver/factory"
	storagemiddleware "github.com/distribution/distribution/v3/registry/storage/driver/middleware"
	"github.com/distribution/distribution/v3/version"
	"github.com/distribution/reference"
	events "github.com/docker/go-events"
	"github.com/docker/go-metrics"
	"github.com/gorilla/mux"
	"github.com/redis/go-redis/extra/redisotel/v9"
	"github.com/redis/go-redis/v9"
	"github.com/sirupsen/logrus"
)

// randomSecretSize is the number of random bytes to generate if no secret
// was specified.
const randomSecretSize = 32

// defaultCheckInterval is the default time in between health checks
const defaultCheckInterval = 10 * time.Second

// App is a global registry application object. Shared resources can be placed
// on this object that will be accessible from all requests. Any writable
// fields should be protected.
type App struct {
	context.Context

	Config *configuration.Configuration

	router           *mux.Router                    // main application router, configured with dispatchers
	driver           storagedriver.StorageDriver    // driver maintains the app global storage driver instance.
	registry         distribution.Namespace         // registry is the primary registry backend for the app instance.
	repoRemover      distribution.RepositoryRemover // repoRemover provides ability to delete repos
	accessController auth.AccessController          // main access controller for application

	// httpHost is a parsed representation of the http.host parameter from
	// the configuration. Only the Scheme and Host fields are used.
	httpHost url.URL

	// events contains notification related configuration.
	events struct {
		sink   events.Sink
		source notifications.SourceRecord
	}

	redis redis.UniversalClient

	// isCache is true if this registry is configured as a pull through cache
	isCache bool

	// readOnly is true if the registry is in a read-only maintenance mode
	readOnly bool
}

// NewApp takes a configuration and returns a configured app, ready to serve
// requests. The app only implements ServeHTTP and can be wrapped in other
// handlers accordingly.
func NewApp(ctx context.Context, config *configuration.Configuration) *App {
	app := &App{
		Config:  config,
		Context: ctx,
		router:  v2.RouterWithPrefix(config.HTTP.Prefix),
		isCache: config.Proxy.RemoteURL != "",
	}

	// Register the handler dispatchers.
	app.register(v2.RouteNameBase, func(ctx *Context, r *http.Request) http.Handler {
		return http.HandlerFunc(apiBase)
	})
	app.register(v2.RouteNameManifest, manifestDispatcher)
	app.register(v2.RouteNameCatalog, catalogDispatcher)
	app.register(v2.RouteNameTags, tagsDispatcher)
	app.register(v2.RouteNameBlob, blobDispatcher)
	app.register(v2.RouteNameBlobUpload, blobUploadDispatcher)
	app.register(v2.RouteNameBlobUploadChunk, blobUploadDispatcher)

	// override the storage driver's UA string for registry outbound HTTP requests
	storageParams := config.Storage.Parameters()
	if storageParams == nil {
		storageParams = make(configuration.Parameters)
	}
	if storageParams["useragent"] == "" {
		storageParams["useragent"] = fmt.Sprintf("distribution/%s %s", version.Version(), runtime.Version())
	}

	var err error
	app.driver, err = factory.Create(app, config.Storage.Type(), storageParams)
	if err != nil {
		// TODO(stevvooe): Move the creation of a service into a protected
		// method, where this is created lazily. Its status can be queried via
		// a health check.
		panic(err)
	}

	purgeConfig := uploadPurgeDefaultConfig()
	if mc, ok := config.Storage["maintenance"]; ok {
		if v, ok := mc["uploadpurging"]; ok {
			purgeConfig, ok = v.(map[interface{}]interface{})
			if !ok {
				panic("uploadpurging config key must contain additional keys")
			}
		}
		if v, ok := mc["readonly"]; ok {
			readOnly, ok := v.(map[interface{}]interface{})
			if !ok {
				panic("readonly config key must contain additional keys")
			}
			if readOnlyEnabled, ok := readOnly["enabled"]; ok {
				app.readOnly, ok = readOnlyEnabled.(bool)
				if !ok {
					panic("readonly's enabled config key must have a boolean value")
				}
			}
		}
	}

	startUploadPurger(app, app.driver, dcontext.GetLogger(app), purgeConfig)

	app.driver, err = applyStorageMiddleware(app, app.driver, config.Middleware["storage"])
	if err != nil {
		panic(err)
	}

	// Do not configure HTTP secret for a proxy registry as HTTP secret
	// is only used for blob uploads and a proxy registry does not support blob uploads.
	if !app.isCache {
		app.configureSecret(config)
	}
	app.configureEvents(config)
	app.configureRedis(config)
	app.configureLogHook(config)

	options := registrymiddleware.GetRegistryOptions()

	if config.HTTP.Host != "" {
		u, err := url.Parse(config.HTTP.Host)
		if err != nil {
			panic(fmt.Sprintf(`could not parse http "host" parameter: %v`, err))
		}
		app.httpHost = *u
	}

	if app.isCache {
		options = append(options, storage.DisableDigestResumption)
	}

	// configure deletion
	if d, ok := config.Storage["delete"]; ok {
		e, ok := d["enabled"]
		if ok {
			if deleteEnabled, ok := e.(bool); ok && deleteEnabled {
				options = append(options, storage.EnableDelete)
			}
		}
	}

	// configure tag lookup concurrency limit
	if p := config.Storage.TagParameters(); p != nil {
		l, ok := p["concurrencylimit"]
		if ok {
			limit, ok := l.(int)
			if !ok {
				panic("tag lookup concurrency limit config key must have a integer value")
			}
			if limit < 0 {
				panic("tag lookup concurrency limit should be a non-negative integer value")
			}
			options = append(options, storage.TagLookupConcurrencyLimit(limit))
		}
	}

	// configure redirects
	var redirectDisabled bool
	if redirectConfig, ok := config.Storage["redirect"]; ok {
		v := redirectConfig["disable"]
		switch v := v.(type) {
		case bool:
			redirectDisabled = v
		default:
			panic(fmt.Sprintf("invalid type for redirect config: %#v", redirectConfig))
		}
	}
	if redirectDisabled {
		dcontext.GetLogger(app).Infof("backend redirection disabled")
	} else {
		options = append(options, storage.EnableRedirect)
	}

	if !config.Validation.Enabled {
		config.Validation.Enabled = !config.Validation.Disabled
	}

	// configure validation
	if config.Validation.Enabled {
		if len(config.Validation.Manifests.URLs.Allow) == 0 && len(config.Validation.Manifests.URLs.Deny) == 0 {
			// If Allow and Deny are empty, allow nothing.
			options = append(options, storage.ManifestURLsAllowRegexp(regexp.MustCompile("^$")))
		} else {
			if len(config.Validation.Manifests.URLs.Allow) > 0 {
				for i, s := range config.Validation.Manifests.URLs.Allow {
					// Validate via compilation.
					if _, err := regexp.Compile(s); err != nil {
						panic(fmt.Sprintf("validation.manifests.urls.allow: %s", err))
					}
					// Wrap with non-capturing group.
					config.Validation.Manifests.URLs.Allow[i] = fmt.Sprintf("(?:%s)", s)
				}
				re := regexp.MustCompile(strings.Join(config.Validation.Manifests.URLs.Allow, "|"))
				options = append(options, storage.ManifestURLsAllowRegexp(re))
			}
			if len(config.Validation.Manifests.URLs.Deny) > 0 {
				for i, s := range config.Validation.Manifests.URLs.Deny {
					// Validate via compilation.
					if _, err := regexp.Compile(s); err != nil {
						panic(fmt.Sprintf("validation.manifests.urls.deny: %s", err))
					}
					// Wrap with non-capturing group.
					config.Validation.Manifests.URLs.Deny[i] = fmt.Sprintf("(?:%s)", s)
				}
				re := regexp.MustCompile(strings.Join(config.Validation.Manifests.URLs.Deny, "|"))
				options = append(options, storage.ManifestURLsDenyRegexp(re))
			}
		}

		switch config.Validation.Manifests.Indexes.Platforms {
		case "list":
			options = append(options, storage.EnableValidateImageIndexImagesExist)
			for _, platform := range config.Validation.Manifests.Indexes.PlatformList {
				options = append(options, storage.AddValidateImageIndexImagesExistPlatform(platform.Architecture, platform.OS))
			}
			fallthrough
		case "none":
			dcontext.GetLogger(app).Warn("Image index completeness validation has been disabled, which is an experimental option because other container tooling might expect all image indexes to be complete")
		case "all":
			fallthrough
		default:
			options = append(options, storage.EnableValidateImageIndexImagesExist)
		}
	}

	// configure storage caches
	if cc, ok := config.Storage["cache"]; ok {
		v, ok := cc["blobdescriptor"]
		if !ok {
			// Backwards compatible: "layerinfo" == "blobdescriptor"
			v = cc["layerinfo"]
		}

		switch v {
		case "redis":
			if app.redis == nil {
				panic("redis configuration required to use for layerinfo cache")
			}
			if _, ok := cc["blobdescriptorsize"]; ok {
				dcontext.GetLogger(app).Warnf("blobdescriptorsize parameter is not supported with redis cache")
			}
			cacheProvider := rediscache.NewRedisBlobDescriptorCacheProvider(app.redis)
			localOptions := append(options, storage.BlobDescriptorCacheProvider(cacheProvider))
			app.registry, err = storage.NewRegistry(app, app.driver, localOptions...)
			if err != nil {
				panic("could not create registry: " + err.Error())
			}
			dcontext.GetLogger(app).Infof("using redis blob descriptor cache")
		case "inmemory":
			blobDescriptorSize := memorycache.DefaultSize
			configuredSize, ok := cc["blobdescriptorsize"]
			if ok {
				// Since Parameters is not strongly typed, render to a string and convert back
				blobDescriptorSize, err = strconv.Atoi(fmt.Sprint(configuredSize))
				if err != nil {
					panic(fmt.Sprintf("invalid blobdescriptorsize value %s: %s", configuredSize, err))
				}
			}

			cacheProvider := memorycache.NewInMemoryBlobDescriptorCacheProvider(blobDescriptorSize)
			localOptions := append(options, storage.BlobDescriptorCacheProvider(cacheProvider))
			app.registry, err = storage.NewRegistry(app, app.driver, localOptions...)
			if err != nil {
				panic("could not create registry: " + err.Error())
			}
			dcontext.GetLogger(app).Infof("using inmemory blob descriptor cache")
		default:
			if v != "" {
				dcontext.GetLogger(app).Warnf("unknown cache type %q, caching disabled", config.Storage["cache"])
			}
		}
	}

	if app.registry == nil {
		// configure the registry if no cache section is available.
		app.registry, err = storage.NewRegistry(app.Context, app.driver, options...)
		if err != nil {
			panic("could not create registry: " + err.Error())
		}
	}

	app.registry, err = applyRegistryMiddleware(app, app.registry, app.driver, config.Middleware["registry"])
	if err != nil {
		panic(err)
	}

	authType := config.Auth.Type()

	if authType != "" && !strings.EqualFold(authType, "none") {
		accessController, err := auth.GetAccessController(config.Auth.Type(), config.Auth.Parameters())
		if err != nil {
			panic(fmt.Sprintf("unable to configure authorization (%s): %v", authType, err))
		}
		app.accessController = accessController
		dcontext.GetLogger(app).Debugf("configured %q access controller", authType)
	}

	// configure as a pull through cache
	if config.Proxy.RemoteURL != "" {
		app.registry, err = proxy.NewRegistryPullThroughCache(ctx, app.registry, app.driver, config.Proxy)
		if err != nil {
			panic(err.Error())
		}
		app.isCache = true
		dcontext.GetLogger(app).Info("Registry configured as a proxy cache to ", config.Proxy.RemoteURL)
	}
	var ok bool
	app.repoRemover, ok = app.registry.(distribution.RepositoryRemover)
	if !ok {
		dcontext.GetLogger(app).Warnf("Registry does not implement RepositoryRemover. Will not be able to delete repos and tags")
	}

	return app
}

// RegisterHealthChecks is an awful hack to defer health check registration
// control to callers. This should only ever be called once per registry
// process, typically in a main function. The correct way would be register
// health checks outside of app, since multiple apps may exist in the same
// process. Because the configuration and app are tightly coupled,
// implementing this properly will require a refactor. This method may panic
// if called twice in the same process.
func (app *App) RegisterHealthChecks(healthRegistries ...*health.Registry) {
	if len(healthRegistries) > 1 {
		panic("RegisterHealthChecks called with more than one registry")
	}
	healthRegistry := health.DefaultRegistry
	if len(healthRegistries) == 1 {
		healthRegistry = healthRegistries[0]
	}

	if app.Config.Health.StorageDriver.Enabled {
		interval := app.Config.Health.StorageDriver.Interval
		if interval == 0 {
			interval = defaultCheckInterval
		}

		storageDriverCheck := health.CheckFunc(func(ctx context.Context) error {
			_, err := app.driver.Stat(ctx, "/") // "/" should always exist
			if _, ok := err.(storagedriver.PathNotFoundError); ok {
				err = nil // pass this through, backend is responding, but this path doesn't exist.
			}
			if err != nil {
				dcontext.GetLogger(ctx).Errorf("storage driver health check: %v", err)
			}
			return err
		})

		updater := health.NewThresholdStatusUpdater(app.Config.Health.StorageDriver.Threshold)
		healthRegistry.Register("storagedriver_"+app.Config.Storage.Type(), updater)
		go health.Poll(app, updater, storageDriverCheck, interval)
	}

	for _, fileChecker := range app.Config.Health.FileCheckers {
		interval := fileChecker.Interval
		if interval == 0 {
			interval = defaultCheckInterval
		}
		dcontext.GetLogger(app).Infof("configuring file health check path=%s, interval=%d", fileChecker.File, interval/time.Second)
		u := health.NewStatusUpdater()
		healthRegistry.Register(fileChecker.File, u)
		go health.Poll(app, u, checks.FileChecker(fileChecker.File), interval)
	}

	for _, httpChecker := range app.Config.Health.HTTPCheckers {
		interval := httpChecker.Interval
		if interval == 0 {
			interval = defaultCheckInterval
		}

		statusCode := httpChecker.StatusCode
		if statusCode == 0 {
			statusCode = 200
		}

		checker := checks.HTTPChecker(httpChecker.URI, statusCode, httpChecker.Timeout, httpChecker.Headers)

		dcontext.GetLogger(app).Infof("configuring HTTP health check uri=%s, interval=%d, threshold=%d", httpChecker.URI, interval/time.Second, httpChecker.Threshold)
		updater := health.NewThresholdStatusUpdater(httpChecker.Threshold)
		healthRegistry.Register(httpChecker.URI, updater)
		go health.Poll(app, updater, checker, interval)
	}

	for _, tcpChecker := range app.Config.Health.TCPCheckers {
		interval := tcpChecker.Interval
		if interval == 0 {
			interval = defaultCheckInterval
		}

		checker := checks.TCPChecker(tcpChecker.Addr, tcpChecker.Timeout)

		dcontext.GetLogger(app).Infof("configuring TCP health check addr=%s, interval=%d, threshold=%d", tcpChecker.Addr, interval/time.Second, tcpChecker.Threshold)
		updater := health.NewThresholdStatusUpdater(tcpChecker.Threshold)
		healthRegistry.Register(tcpChecker.Addr, updater)
		go health.Poll(app, updater, checker, interval)
	}
}

// Shutdown close the underlying registry
func (app *App) Shutdown() error {
	if r, ok := app.registry.(proxy.Closer); ok {
		return r.Close()
	}
	return nil
}

// register a handler with the application, by route name. The handler will be
// passed through the application filters and context will be constructed at
// request time.
func (app *App) register(routeName string, dispatch dispatchFunc) {
	handler := app.dispatcher(dispatch)

	// Chain the handler with prometheus instrumented handler
	if app.Config.HTTP.Debug.Prometheus.Enabled {
		namespace := metrics.NewNamespace(prometheus.NamespacePrefix, "http", nil)
		httpMetrics := namespace.NewDefaultHttpMetrics(strings.Replace(routeName, "-", "_", -1))
		metrics.Register(namespace)
		handler = metrics.InstrumentHandler(httpMetrics, handler)
	}

	// TODO(stevvooe): This odd dispatcher/route registration is by-product of
	// some limitations in the gorilla/mux router. We are using it to keep
	// routing consistent between the client and server, but we may want to
	// replace it with manual routing and structure-based dispatch for better
	// control over the request execution.

	app.router.GetRoute(routeName).Handler(handler)
}

// configureEvents prepares the event sink for action.
func (app *App) configureEvents(configuration *configuration.Configuration) {
	// Configure all of the endpoint sinks.
	// NOTE(milosgajdos): we are disabling the linter here as
	// if an endpoint is disabled we continue with the evaluation
	// of the next one so we do not know the exact size the slice
	// should have at the time the iteration starts
	// nolint:prealloc
	var sinks []events.Sink
	for _, endpoint := range configuration.Notifications.Endpoints {
		if endpoint.Disabled {
			dcontext.GetLogger(app).Infof("endpoint %s disabled, skipping", endpoint.Name)
			continue
		}

		dcontext.GetLogger(app).Infof("configuring endpoint %v (%v), timeout=%s, headers=%v", endpoint.Name, endpoint.URL, endpoint.Timeout, endpoint.Headers)
		endpoint := notifications.NewEndpoint(endpoint.Name, endpoint.URL, notifications.EndpointConfig{
			Timeout:           endpoint.Timeout,
			Threshold:         endpoint.Threshold,
			Backoff:           endpoint.Backoff,
			Headers:           endpoint.Headers,
			IgnoredMediaTypes: endpoint.IgnoredMediaTypes,
			Ignore:            endpoint.Ignore,
		})

		sinks = append(sinks, endpoint)
	}

	// NOTE(stevvooe): Moving to a new queuing implementation is as easy as
	// replacing broadcaster with a rabbitmq implementation. It's recommended
	// that the registry instances also act as the workers to keep deployment
	// simple.
	app.events.sink = events.NewBroadcaster(sinks...)

	// Populate registry event source
	hostname, err := os.Hostname()
	if err != nil {
		hostname = configuration.HTTP.Addr
	} else {
		// try to pick the port off the config
		_, port, err := net.SplitHostPort(configuration.HTTP.Addr)
		if err == nil {
			hostname = net.JoinHostPort(hostname, port)
		}
	}

	app.events.source = notifications.SourceRecord{
		Addr:       hostname,
		InstanceID: dcontext.GetStringValue(app, "instance.id"),
	}
}

func (app *App) configureRedis(cfg *configuration.Configuration) {
	if len(cfg.Redis.Options.Addrs) == 0 {
		dcontext.GetLogger(app).Infof("redis not configured")
		return
	}

	opts := redis.UniversalOptions{
		Addrs:                 cfg.Redis.Options.Addrs,
		ClientName:            cfg.Redis.Options.ClientName,
		DB:                    cfg.Redis.Options.DB,
		Protocol:              cfg.Redis.Options.Protocol,
		Username:              cfg.Redis.Options.Username,
		Password:              cfg.Redis.Options.Password,
		SentinelUsername:      cfg.Redis.Options.SentinelUsername,
		SentinelPassword:      cfg.Redis.Options.SentinelPassword,
		MaxRetries:            cfg.Redis.Options.MaxRetries,
		MinRetryBackoff:       cfg.Redis.Options.MinRetryBackoff,
		MaxRetryBackoff:       cfg.Redis.Options.MaxRetryBackoff,
		DialTimeout:           cfg.Redis.Options.DialTimeout,
		ReadTimeout:           cfg.Redis.Options.ReadTimeout,
		WriteTimeout:          cfg.Redis.Options.WriteTimeout,
		ContextTimeoutEnabled: cfg.Redis.Options.ContextTimeoutEnabled,
		PoolFIFO:              cfg.Redis.Options.PoolFIFO,
		PoolSize:              cfg.Redis.Options.PoolSize,
		PoolTimeout:           cfg.Redis.Options.PoolTimeout,
		MinIdleConns:          cfg.Redis.Options.MinIdleConns,
		MaxIdleConns:          cfg.Redis.Options.MaxIdleConns,
		MaxActiveConns:        cfg.Redis.Options.MaxActiveConns,
		ConnMaxIdleTime:       cfg.Redis.Options.ConnMaxIdleTime,
		ConnMaxLifetime:       cfg.Redis.Options.ConnMaxLifetime,
		MaxRedirects:          cfg.Redis.Options.MaxRedirects,
		ReadOnly:              cfg.Redis.Options.ReadOnly,
		RouteByLatency:        cfg.Redis.Options.RouteByLatency,
		RouteRandomly:         cfg.Redis.Options.RouteRandomly,
		MasterName:            cfg.Redis.Options.MasterName,
		DisableIdentity:       cfg.Redis.Options.DisableIdentity,
		IdentitySuffix:        cfg.Redis.Options.IdentitySuffix,
		UnstableResp3:         cfg.Redis.Options.UnstableResp3,
	}

	// redis TLS config
	if cfg.Redis.TLS.Certificate != "" || cfg.Redis.TLS.Key != "" {
		var err error
		tlsConf := &tls.Config{}
		tlsConf.Certificates = make([]tls.Certificate, 1)
		tlsConf.Certificates[0], err = tls.LoadX509KeyPair(cfg.Redis.TLS.Certificate, cfg.Redis.TLS.Key)
		if err != nil {
			panic(err)
		}
		if len(cfg.Redis.TLS.ClientCAs) != 0 {
			pool := x509.NewCertPool()
			for _, ca := range cfg.Redis.TLS.ClientCAs {
				caPem, err := os.ReadFile(ca)
				if err != nil {
					dcontext.GetLogger(app).Errorf("failed reading redis client CA: %v", err)
					return
				}

				if ok := pool.AppendCertsFromPEM(caPem); !ok {
					dcontext.GetLogger(app).Error("could not add CA to pool")
					return
				}
			}
			tlsConf.ClientAuth = tls.RequireAndVerifyClientCert
			tlsConf.ClientCAs = pool
		}
		opts.TLSConfig = tlsConf
	}

	app.redis = app.createPool(opts)

	// Enable metrics instrumentation.
	if err := redisotel.InstrumentMetrics(app.redis); err != nil {
		dcontext.GetLogger(app).Errorf("failed to instrument metrics on redis: %v", err)
	}

	// setup expvar
	registry := expvar.Get("registry")
	if registry == nil {
		registry = expvar.NewMap("registry")
	}

	registry.(*expvar.Map).Set("redis", expvar.Func(func() interface{} {
		stats := app.redis.PoolStats()
		return map[string]interface{}{
			"Config": cfg,
			"Active": stats.TotalConns - stats.IdleConns,
		}
	}))
}

func (app *App) createPool(cfg redis.UniversalOptions) redis.UniversalClient {
	cfg.OnConnect = func(ctx context.Context, cn *redis.Conn) error {
		res := cn.Ping(ctx)
		return res.Err()
	}
	return redis.NewUniversalClient(&cfg)
}

// configureLogHook prepares logging hook parameters.
func (app *App) configureLogHook(configuration *configuration.Configuration) {
	entry, ok := dcontext.GetLogger(app).(*logrus.Entry)
	if !ok {
		// somehow, we are not using logrus
		return
	}

	logger := entry.Logger

	for _, configHook := range configuration.Log.Hooks {
		if !configHook.Disabled {
			switch configHook.Type {
			case "mail":
				hook := &logHook{}
				hook.LevelsParam = configHook.Levels
				hook.Mail = &mailer{
					Addr:     configHook.MailOptions.SMTP.Addr,
					Username: configHook.MailOptions.SMTP.Username,
					Password: configHook.MailOptions.SMTP.Password,
					Insecure: configHook.MailOptions.SMTP.Insecure,
					From:     configHook.MailOptions.From,
					To:       configHook.MailOptions.To,
				}
				logger.Hooks.Add(hook)
			default:
			}
		}
	}
}

// configureSecret creates a random secret if a secret wasn't included in the
// configuration.
func (app *App) configureSecret(configuration *configuration.Configuration) {
	if configuration.HTTP.Secret == "" {
		var secretBytes [randomSecretSize]byte
		if _, err := rand.Read(secretBytes[:]); err != nil {
			panic(fmt.Sprintf("could not generate random bytes for HTTP secret: %v", err))
		}
		configuration.HTTP.Secret = string(secretBytes[:])
		dcontext.GetLogger(app).Warn("No HTTP secret provided - generated random secret. This may cause problems with uploads if multiple registries are behind a load-balancer. To provide a shared secret, fill in http.secret in the configuration file or set the REGISTRY_HTTP_SECRET environment variable.")
	}
}

func (app *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Prepare the context with our own little decorations.
	ctx := r.Context()
	ctx = dcontext.WithRequest(ctx, r)
	ctx, w = dcontext.WithResponseWriter(ctx, w)
	ctx = dcontext.WithLogger(ctx, dcontext.GetRequestLogger(ctx))
	r = r.WithContext(ctx)

	// Set a header with the Docker Distribution API Version for all responses.
	w.Header().Add("Docker-Distribution-API-Version", "registry/2.0")
	app.router.ServeHTTP(w, r)
}

// dispatchFunc takes a context and request and returns a constructed handler
// for the route. The dispatcher will use this to dynamically create request
// specific handlers for each endpoint without creating a new router for each
// request.
type dispatchFunc func(ctx *Context, r *http.Request) http.Handler

// TODO(stevvooe): dispatchers should probably have some validation error
// chain with proper error reporting.

// dispatcher returns a handler that constructs a request specific context and
// handler, using the dispatch factory function.
func (app *App) dispatcher(dispatch dispatchFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for headerName, headerValues := range app.Config.HTTP.Headers {
			for _, value := range headerValues {
				w.Header().Add(headerName, value)
			}
		}

		context := app.context(w, r)

		defer func() {
			// Automated error response handling here. Handlers may return their
			// own errors if they need different behavior (such as range errors
			// for layer upload).
			if context.Errors.Len() > 0 {
				_ = errcode.ServeJSON(w, context.Errors)
				app.logError(context, context.Errors)
			} else if status, ok := context.Value("http.response.status").(int); ok && status >= 200 && status <= 399 {
				dcontext.GetResponseLogger(context).Infof("response completed")
			}
		}()

		if err := app.authorized(w, r, context); err != nil {
			dcontext.GetLogger(context).Warnf("error authorizing context: %v", err)
			return
		}

		// Add username to request logging
		context.Context = dcontext.WithLogger(context.Context, dcontext.GetLogger(context.Context, userNameKey))

		// sync up context on the request.
		r = r.WithContext(context)

		if app.nameRequired(r) {
			nameRef, err := reference.WithName(getName(context))
			if err != nil {
				dcontext.GetLogger(context).Errorf("error parsing reference from context: %v", err)
				context.Errors = append(context.Errors, distribution.ErrRepositoryNameInvalid{
					Name:   getName(context),
					Reason: err,
				})
				if err := errcode.ServeJSON(w, context.Errors); err != nil {
					dcontext.GetLogger(context).Errorf("error serving error json: %v (from %v)", err, context.Errors)
				}
				return
			}
			repository, err := app.registry.Repository(context, nameRef)
			if err != nil {
				dcontext.GetLogger(context).Errorf("error resolving repository: %v", err)

				switch err := err.(type) {
				case distribution.ErrRepositoryUnknown:
					context.Errors = append(context.Errors, errcode.ErrorCodeNameUnknown.WithDetail(err))
				case distribution.ErrRepositoryNameInvalid:
					context.Errors = append(context.Errors, errcode.ErrorCodeNameInvalid.WithDetail(err))
				case errcode.Error:
					context.Errors = append(context.Errors, err)
				}

				if err := errcode.ServeJSON(w, context.Errors); err != nil {
					dcontext.GetLogger(context).Errorf("error serving error json: %v (from %v)", err, context.Errors)
				}
				return
			}

			// assign and decorate the authorized repository with an event bridge.
			context.Repository, context.RepositoryRemover = notifications.Listen(
				repository,
				context.App.repoRemover,
				app.eventBridge(context, r))

			context.Repository, err = applyRepoMiddleware(app, context.Repository, app.Config.Middleware["repository"])
			if err != nil {
				dcontext.GetLogger(context).Errorf("error initializing repository middleware: %v", err)
				context.Errors = append(context.Errors, errcode.ErrorCodeUnknown.WithDetail(err))

				if err := errcode.ServeJSON(w, context.Errors); err != nil {
					dcontext.GetLogger(context).Errorf("error serving error json: %v (from %v)", err, context.Errors)
				}
				return
			}
		}

		dispatch(context, r).ServeHTTP(w, r)
	})
}

type errCodeKey struct{}

func (errCodeKey) String() string { return "err.code" }

type errMessageKey struct{}

func (errMessageKey) String() string { return "err.message" }

type errDetailKey struct{}

func (errDetailKey) String() string { return "err.detail" }

func (app *App) logError(ctx context.Context, errors errcode.Errors) {
	for _, e1 := range errors {
		var c context.Context

		switch e := e1.(type) {
		case errcode.Error:
			c = context.WithValue(ctx, errCodeKey{}, e.Code)
			c = context.WithValue(c, errMessageKey{}, e.Message)
			c = context.WithValue(c, errDetailKey{}, e.Detail)
		case errcode.ErrorCode:
			c = context.WithValue(ctx, errCodeKey{}, e)
			c = context.WithValue(c, errMessageKey{}, e.Message())
		default:
			// just normal go 'error'
			c = context.WithValue(ctx, errCodeKey{}, errcode.ErrorCodeUnknown)
			c = context.WithValue(c, errMessageKey{}, e.Error())
		}

		c = dcontext.WithLogger(c, dcontext.GetLogger(c,
			errCodeKey{},
			errMessageKey{},
			errDetailKey{}))
		dcontext.GetResponseLogger(c).Errorf("response completed with error")
	}
}

// context constructs the context object for the application. This only be
// called once per request.
func (app *App) context(w http.ResponseWriter, r *http.Request) *Context {
	ctx := r.Context()
	ctx = dcontext.WithVars(ctx, r)
	ctx = dcontext.WithLogger(ctx, dcontext.GetLogger(ctx,
		"vars.name",
		"vars.reference",
		"vars.digest",
		"vars.uuid"))

	context := &Context{
		App:     app,
		Context: ctx,
	}

	if app.httpHost.Scheme != "" && app.httpHost.Host != "" {
		// A "host" item in the configuration takes precedence over
		// X-Forwarded-Proto and X-Forwarded-Host headers, and the
		// hostname in the request.
		context.urlBuilder = v2.NewURLBuilder(&app.httpHost, false)
	} else {
		context.urlBuilder = v2.NewURLBuilderFromRequest(r, app.Config.HTTP.RelativeURLs)
	}

	return context
}

// authorized checks if the request can proceed with access to the requested
// repository. If it succeeds, the context may access the requested
// repository. An error will be returned if access is not available.
func (app *App) authorized(w http.ResponseWriter, r *http.Request, context *Context) error {
	dcontext.GetLogger(context).Debug("authorizing request")
	repo := getName(context)

	if app.accessController == nil {
		return nil // access controller is not enabled.
	}

	var accessRecords []auth.Access

	if repo != "" {
		accessRecords = appendAccessRecords(accessRecords, r.Method, repo)
		if fromRepo := r.FormValue("from"); fromRepo != "" {
			// mounting a blob from one repository to another requires pull (GET)
			// access to the source repository.
			accessRecords = appendAccessRecords(accessRecords, http.MethodGet, fromRepo)
		}
	} else {
		// Only allow the name not to be set on the base route.
		if app.nameRequired(r) {
			// For this to be properly secured, repo must always be set for a
			// resource that may make a modification. The only condition under
			// which name is not set and we still allow access is when the
			// base route is accessed. This section prevents us from making
			// that mistake elsewhere in the code, allowing any operation to
			// proceed.
			if err := errcode.ServeJSON(w, errcode.ErrorCodeUnauthorized); err != nil {
				dcontext.GetLogger(context).Errorf("error serving error json: %v (from %v)", err, context.Errors)
			}
			return fmt.Errorf("forbidden: no repository name")
		}
		accessRecords = appendCatalogAccessRecord(accessRecords, r)
	}

	grant, err := app.accessController.Authorized(r.WithContext(context.Context), accessRecords...)
	if err != nil {
		switch err := err.(type) {
		case auth.Challenge:
			// Add the appropriate WWW-Auth header
			err.SetHeaders(r, w)

			if err := errcode.ServeJSON(w, errcode.ErrorCodeUnauthorized.WithDetail(accessRecords)); err != nil {
				dcontext.GetLogger(context).Errorf("error serving error json: %v (from %v)", err, context.Errors)
			}
		default:
			// This condition is a potential security problem either in
			// the configuration or whatever is backing the access
			// controller. Just return a bad request with no information
			// to avoid exposure. The request should not proceed.
			dcontext.GetLogger(context).Errorf("error checking authorization: %v", err)
			w.WriteHeader(http.StatusBadRequest)
		}

		return err
	}
	if grant == nil {
		return fmt.Errorf("access controller returned neither an access grant nor an error")
	}

	ctx := withUser(context.Context, grant.User)
	ctx = withResources(ctx, grant.Resources)

	dcontext.GetLogger(ctx, userNameKey).Info("authorized request")
	// TODO(stevvooe): This pattern needs to be cleaned up a bit. One context
	// should be replaced by another, rather than replacing the context on a
	// mutable object.
	context.Context = ctx
	return nil
}

// eventBridge returns a bridge for the current request, configured with the
// correct actor and source.
func (app *App) eventBridge(ctx *Context, r *http.Request) notifications.Listener {
	actor := notifications.ActorRecord{
		Name: getUserName(ctx, r),
	}
	request := notifications.NewRequestRecord(dcontext.GetRequestID(ctx), r)

	return notifications.NewBridge(ctx.urlBuilder, app.events.source, actor, request, app.events.sink, app.Config.Notifications.EventConfig.IncludeReferences)
}

// nameRequired returns true if the route requires a name.
func (app *App) nameRequired(r *http.Request) bool {
	route := mux.CurrentRoute(r)
	if route == nil {
		return true
	}
	routeName := route.GetName()
	return routeName != v2.RouteNameBase && routeName != v2.RouteNameCatalog
}

// apiBase implements a simple yes-man for doing overall checks against the
// api. This can support auth roundtrips to support docker login.
func apiBase(w http.ResponseWriter, r *http.Request) {
	const emptyJSON = "{}"
	// Provide a simple /v2/ 200 OK response with empty json response.
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", fmt.Sprint(len(emptyJSON)))

	fmt.Fprint(w, emptyJSON)
}

// appendAccessRecords checks the method and adds the appropriate Access records to the records list.
func appendAccessRecords(records []auth.Access, method string, repo string) []auth.Access {
	resource := auth.Resource{
		Type: "repository",
		Name: repo,
	}

	switch method {
	case http.MethodGet, http.MethodHead:
		records = append(records,
			auth.Access{
				Resource: resource,
				Action:   "pull",
			})
	case http.MethodPost, http.MethodPut, http.MethodPatch:
		records = append(records,
			auth.Access{
				Resource: resource,
				Action:   "pull",
			},
			auth.Access{
				Resource: resource,
				Action:   "push",
			})
	case http.MethodDelete:
		records = append(records,
			auth.Access{
				Resource: resource,
				Action:   "delete",
			})
	}
	return records
}

// Add the access record for the catalog if it's our current route
func appendCatalogAccessRecord(accessRecords []auth.Access, r *http.Request) []auth.Access {
	route := mux.CurrentRoute(r)
	routeName := route.GetName()

	if routeName == v2.RouteNameCatalog {
		resource := auth.Resource{
			Type: "registry",
			Name: "catalog",
		}

		accessRecords = append(accessRecords,
			auth.Access{
				Resource: resource,
				Action:   "*",
			})
	}
	return accessRecords
}

// applyRegistryMiddleware wraps a registry instance with the configured middlewares
func applyRegistryMiddleware(ctx context.Context, registry distribution.Namespace, driver storagedriver.StorageDriver, middlewares []configuration.Middleware) (distribution.Namespace, error) {
	for _, mw := range middlewares {
		rmw, err := registrymiddleware.Get(ctx, mw.Name, mw.Options, registry, driver)
		if err != nil {
			return nil, fmt.Errorf("unable to configure registry middleware (%s): %s", mw.Name, err)
		}
		registry = rmw
	}
	return registry, nil
}

// applyRepoMiddleware wraps a repository with the configured middlewares
func applyRepoMiddleware(ctx context.Context, repository distribution.Repository, middlewares []configuration.Middleware) (distribution.Repository, error) {
	for _, mw := range middlewares {
		rmw, err := repositorymiddleware.Get(ctx, mw.Name, mw.Options, repository)
		if err != nil {
			return nil, err
		}
		repository = rmw
	}
	return repository, nil
}

// applyStorageMiddleware wraps a storage driver with the configured middlewares
func applyStorageMiddleware(ctx context.Context, driver storagedriver.StorageDriver, middlewares []configuration.Middleware) (storagedriver.StorageDriver, error) {
	for _, mw := range middlewares {
		smw, err := storagemiddleware.Get(ctx, mw.Name, mw.Options, driver)
		if err != nil {
			return nil, fmt.Errorf("unable to configure storage middleware (%s): %v", mw.Name, err)
		}
		driver = smw
	}
	return driver, nil
}

// uploadPurgeDefaultConfig provides a default configuration for upload
// purging to be used in the absence of configuration in the
// configuration file
func uploadPurgeDefaultConfig() map[interface{}]interface{} {
	config := map[interface{}]interface{}{}
	config["enabled"] = true
	config["age"] = "168h"
	config["interval"] = "24h"
	config["dryrun"] = false
	return config
}

func badPurgeUploadConfig(reason string) {
	panic(fmt.Sprintf("Unable to parse upload purge configuration: %s", reason))
}

// startUploadPurger schedules a goroutine which will periodically
// check upload directories for old files and delete them
func startUploadPurger(ctx context.Context, storageDriver storagedriver.StorageDriver, log dcontext.Logger, config map[interface{}]interface{}) {
	if config["enabled"] == false {
		return
	}

	var purgeAgeDuration time.Duration
	var err error
	purgeAge, ok := config["age"]
	if ok {
		ageStr, ok := purgeAge.(string)
		if !ok {
			badPurgeUploadConfig("age is not a string")
		}
		purgeAgeDuration, err = time.ParseDuration(ageStr)
		if err != nil {
			badPurgeUploadConfig(fmt.Sprintf("Cannot parse duration: %s", err.Error()))
		}
	} else {
		badPurgeUploadConfig("age missing")
	}

	var intervalDuration time.Duration
	interval, ok := config["interval"]
	if ok {
		intervalStr, ok := interval.(string)
		if !ok {
			badPurgeUploadConfig("interval is not a string")
		}

		intervalDuration, err = time.ParseDuration(intervalStr)
		if err != nil {
			badPurgeUploadConfig(fmt.Sprintf("Cannot parse interval: %s", err.Error()))
		}
	} else {
		badPurgeUploadConfig("interval missing")
	}

	var dryRunBool bool
	dryRun, ok := config["dryrun"]
	if ok {
		dryRunBool, ok = dryRun.(bool)
		if !ok {
			badPurgeUploadConfig("cannot parse dryrun")
		}
	} else {
		badPurgeUploadConfig("dryrun missing")
	}

	go func() {
		randInt, err := rand.Int(rand.Reader, new(big.Int).SetInt64(math.MaxInt64))
		if err != nil {
			log.Infof("Failed to generate random jitter: %v", err)
			// sleep 30min for failure case
			randInt = big.NewInt(30)
		}
		jitter := time.Duration(randInt.Int64()%60) * time.Minute
		log.Infof("Starting upload purge in %s", jitter)
		time.Sleep(jitter)

		for {
			storage.PurgeUploads(ctx, storageDriver, time.Now().Add(-purgeAgeDuration), !dryRunBool)
			log.Infof("Starting upload purge in %s", intervalDuration)
			time.Sleep(intervalDuration)
		}
	}()
}
