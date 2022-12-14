package main

import (
	"context"
	"database/sql"
	"flag"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/bots-house/share-file-bot/bot"
	"github.com/bots-house/share-file-bot/bot/state"
	"github.com/bots-house/share-file-bot/pkg/health"
	"github.com/bots-house/share-file-bot/service"
	"github.com/bots-house/share-file-bot/store/postgres"
	"github.com/friendsofgo/errors"
	"github.com/getsentry/sentry-go"
	sentryhttp "github.com/getsentry/sentry-go/http"
	redis "github.com/go-redis/redis/v8"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api"

	"github.com/kelseyhightower/envconfig"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/subosito/gotenv"
)

const (
	EnvLocal      = "local"
	EnvStaging    = "staging"
	EnvProduction = "production"
)

// Config represents service configuration
type Config struct {
	Env string `split_words:"true" default:"local"`

	SentryDSN string `split_words:"true"`

	Database             string `default:"postgres://sfb:sfb@localhost/sfb?sslmode=disable"`
	DatabaseMaxOpenConns int    `default:"10" split_words:"true"`
	DatabaseMaxIdleConns int    `default:"0" split_words:"true"`

	Redis             string `default:"redis://localhost:6379"`
	RedisMaxOpenConns int    `default:"10" split_words:"true"`
	RedisMaxIdleConns int    `default:"0" split_words:"true"`

	Token        string `required:"true"`
	Addr         string `default:":8000"`
	WebhookURL   string `default:"/" split_words:"true"`
	SecretIDSalt string `required:"true" split_words:"true"`

	DryRun bool `default:"false" split_words:"true"`

	LogDebug  bool `default:"true" split_words:"true"`
	LogPretty bool `default:"false" split_words:"true"`
}

func (cfg Config) getEnv() string {
	for _, v := range []string{EnvLocal, EnvProduction, EnvStaging} {
		if v == strings.ToLower(cfg.Env) {
			return v
		}
	}
	return EnvLocal
}

var revision = "unknown"

func main() {
	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	go func() {
		sig := make(chan os.Signal, 1)

		signal.Notify(sig, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)

		<-sig

		cancel()
	}()

	if err := run(ctx); err != nil {
		log.Error().Err(err).Msg("fatal error")
		cancel()
		//nolint: gocritic
		os.Exit(1)
	}
}

func newServer(addr string, bot *bot.Bot, db *sql.DB, cfg Config) *http.Server {
	baseCtx := context.Background()
	baseCtx = setupLogging(baseCtx, cfg)

	sentryMiddleware := sentryhttp.New(sentryhttp.Options{
		Repanic: true,
	})

	return &http.Server{
		Addr:    addr,
		Handler: sentryMiddleware.Handle(newMux(bot, db)),
		BaseContext: func(_ net.Listener) context.Context {
			return baseCtx
		},
	}
}

func newMux(bot *bot.Bot, db *sql.DB) *http.ServeMux {
	mux := http.NewServeMux()

	mux.Handle("/health", health.NewHandler(db))

	mux.Handle("/", bot)

	return mux
}

func newSentry(ctx context.Context, cfg Config, release string) error {
	env := cfg.getEnv()

	if env == EnvLocal {
		log.Ctx(ctx).Debug().Str("env", env).Msg("sentry is not available in this env")
		return nil
	}

	if cfg.SentryDSN == "" {
		log.Ctx(ctx).Warn().Str("env", env).Msg("sentry dsn is not provided")
		return nil
	}

	if err := sentry.Init(sentry.ClientOptions{
		Dsn:         cfg.SentryDSN,
		Environment: cfg.Env,
		Release:     release,
	}); err != nil {
		return errors.Wrap(err, "init sentry")
	}

	return nil
}

const envPrefix = "SFB"

func run(ctx context.Context) error {
	// parse config
	var cfg Config

	// parse flags
	var (
		flagHealth bool
		flagConfig string
	)

	flag.BoolVar(&flagHealth, "health", false, "run health check")
	flag.StringVar(&flagConfig, "config", "", "load env from file")

	flag.Parse()

	if flagHealth {
		return health.Check(ctx, cfg.Addr)
	}

	// parse config
	cfg, err := parseConfig(flagConfig)
	if err != nil {
		return err
	}

	ctx = setupLogging(ctx, cfg)

	log.Ctx(ctx).Info().Str("revision", revision).Msg("start")
	if err := newSentry(ctx, cfg, revision); err != nil {
		return errors.Wrap(err, "init sentry")
	}

	log.Ctx(ctx).Info().Str("dsn", cfg.Database).Msg("open db")

	// open and ping db
	db, err := sql.Open("postgres", cfg.Database)
	if err != nil {
		return errors.Wrap(err, "open db")
	}
	defer db.Close()

	log.Ctx(ctx).Debug().Msg("ping database")
	if err := db.PingContext(ctx); err != nil {
		return errors.Wrap(err, "ping db")
	}

	db.SetMaxOpenConns(cfg.DatabaseMaxOpenConns)
	db.SetMaxIdleConns(cfg.DatabaseMaxIdleConns)

	// create abstraction around db and apply migrations
	pg := postgres.New(db)

	log.Ctx(ctx).Info().Msg("migrate database")
	if err := pg.Migrator().Up(ctx); err != nil {
		return errors.Wrap(err, "migrate db")
	}

	log.Ctx(ctx).Info().Str("dsn", cfg.Redis).Int("max_open_conns", cfg.RedisMaxOpenConns).Int("max_idle_conns", cfg.RedisMaxIdleConns).Msg("open redis")
	rdbOpts, err := redis.ParseURL(cfg.Redis)
	if err != nil {
		return errors.Wrap(err, "parse redis url")
	}

	rdb := redis.NewClient(rdbOpts)

	log.Ctx(ctx).Debug().Msg("ping redis")
	if _, err := rdb.Ping(ctx).Result(); err != nil {
		return errors.Wrap(err, "ping redis")
	}

	botState := state.NewRedisStore(rdb, "share-file-bot")

	log.Ctx(ctx).Info().Msg("init bot api client")
	tgClient, err := tgbotapi.NewBotAPI(cfg.Token)
	if err != nil {
		return errors.Wrap(err, "create bot api")
	}

	authSrv := &service.Auth{
		UserStore: pg.User(),
	}

	fileSrv := &service.File{
		File:     pg.File(),
		Chat:     pg.Chat(),
		Download: pg.Download(),
		Telegram: tgClient,
		Redis:    rdb,
	}

	adminSrv := &service.Admin{
		User:     pg.User(),
		File:     pg.File(),
		Download: pg.Download(),
		Chat:     pg.Chat(),
	}

	chatSrv := &service.Chat{
		Telegram: tgClient,
		Txier:    pg.Tx,
		Chat:     pg.Chat(),
		File:     pg.File(),
		Download: pg.Download(),
	}

	log.Ctx(ctx).Info().Msg("init bot")
	tgBot, err := bot.New(revision, tgClient, botState, authSrv, fileSrv, adminSrv, chatSrv)
	if err != nil {
		return errors.Wrap(err, "init bot")
	}

	log.Ctx(ctx).Info().Str("link", "https://t.me/"+tgBot.Self().UserName).Msg("bot is alive")

	server := newServer(cfg.Addr, tgBot, db, cfg)

	go func() {
		<-ctx.Done()

		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second*10)
		defer cancel()

		log.Ctx(ctx).Info().Msg("shutdown server")
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Ctx(ctx).Warn().Err(err).Msg("shutdown error")
		}
	}()

	if err := tgBot.SetWebhookIfNeed(ctx, cfg.WebhookURL); err != nil {
		return errors.Wrap(err, "set webhook if need")
	}

	// if we run in dry run mode, exit without blocking
	if cfg.DryRun {
		return nil
	}

	log.Ctx(ctx).Info().Str("addr", cfg.Addr).Str("webhook_domain", cfg.WebhookURL).Msg("start server")
	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		return errors.Wrap(err, "listen and serve")
	}

	return nil
}

func parseConfig(config string) (Config, error) {
	var cfg Config

	// load envs
	if config != "" {
		if err := gotenv.Load(config); err != nil {
			return cfg, errors.Wrap(err, "load env")
		}
	}

	if err := envconfig.Process(envPrefix, &cfg); err != nil {
		_ = envconfig.Usage(envPrefix, &cfg)
		return cfg, err
	}

	return cfg, nil
}

func setupLogging(ctx context.Context, cfg Config) context.Context {
	if cfg.LogPretty {
		log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stdout})
	}

	zerolog.SetGlobalLevel(zerolog.InfoLevel)

	if cfg.LogDebug {
		zerolog.SetGlobalLevel(zerolog.DebugLevel)
	}

	return log.Logger.WithContext(ctx)
}
