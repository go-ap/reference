package app

import (
	"context"
	"io"
	"syscall"

	w "git.sr.ht/~mariusor/wrapper"
	pub "github.com/go-ap/activitypub"
	"github.com/go-ap/auth"
	"github.com/go-ap/errors"
	ap "github.com/go-ap/fedbox/activitypub"
	"github.com/go-ap/fedbox/internal/cache"
	"github.com/go-ap/fedbox/internal/config"
	"github.com/go-ap/fedbox/internal/log"
	"github.com/go-ap/handlers"
	st "github.com/go-ap/storage"
	"github.com/go-chi/chi"
	"github.com/go-chi/chi/middleware"
	"github.com/openshift/osin"
	"github.com/sirupsen/logrus"
)

var Config config.Options

type LogFn func(string, ...interface{})

type FedBOX struct {
	conf         config.Options
	R            chi.Router
	ver          string
	caches       cache.CanStore
	Storage      st.Store
	OAuthStorage osin.Storage
	stopFn       func()
	infFn        LogFn
	errFn        LogFn
}

var (
	emptyFieldsLogFn = func(logrus.Fields, string, ...interface{}) {}
	emptyLogFn       = func(string, ...interface{}) {}
	InfoLogFn        = func(l logrus.FieldLogger) func(logrus.Fields, string, ...interface{}) {
		if l == nil {
			return emptyFieldsLogFn
		}
		return func(f logrus.Fields, s string, p ...interface{}) { l.WithFields(f).Infof(s, p...) }
	}
	ErrLogFn = func(l logrus.FieldLogger) func(logrus.Fields, string, ...interface{}) {
		if l == nil {
			return emptyFieldsLogFn
		}
		return func(f logrus.Fields, s string, p ...interface{}) { l.WithFields(f).Errorf(s, p...) }
	}
)

var AnonymousAcct = account{
	username: "anonymous",
	actor:    &auth.AnonymousActor,
}

var InternalIRI = pub.IRI("https://fedbox/")

// New instantiates a new FedBOX instance
func New(l logrus.FieldLogger, ver string, conf config.Options, db st.Store, o osin.Storage) (*FedBOX, error) {
	app := FedBOX{
		ver:          ver,
		conf:         conf,
		R:            chi.NewRouter(),
		Storage:      db,
		OAuthStorage: o,
		infFn:        emptyLogFn,
		errFn:        emptyLogFn,
		caches:       cache.New(!(conf.Env.IsTest() || conf.Env.IsDev())),
	}
	if l != nil {
		app.infFn = l.Infof
		app.errFn = l.Errorf
	}
	Config = conf
	ap.Secure = conf.Secure
	errors.IncludeBacktrace = conf.Env.IsDev() || conf.Env.IsTest()

	osin, err := auth.NewServer(app.OAuthStorage, l)
	if err != nil {
		l.Warn(err.Error())
		return nil, err
	}

	app.R.Use(Repo(db))
	app.R.Use(middleware.RequestID)
	app.R.Use(log.NewStructuredLogger(l))
	app.R.Route("/", app.Routes(Config.BaseURL, osin, l))

	return &app, err
}

func (f FedBOX) Config() config.Options {
	return f.conf
}

// Stop
func (f *FedBOX) Stop() {
	if f.stopFn != nil {
		f.stopFn()
	}
}

// Run is the wrapper for starting the web-server and handling signals
func (f *FedBOX) Run() error {
	// Create a deadline to wait for.
	ctx, cancel := context.WithTimeout(context.TODO(), f.conf.TimeOut)
	defer cancel()

	// set local path typer to validate collections
	handlers.Typer = pathTyper{}

	listenOn := "HTTP"
	if len(f.conf.CertPath) + len(f.conf.KeyPath) > 0 {
		listenOn = "HTTPS"
	}
	// Get start/stop functions for the http server
	srvRun, srvStop := w.HttpServer(ctx, w.Handler(f.R), w.ListenOn(f.conf.Listen), w.SSL(f.conf.CertPath, f.conf.KeyPath))
	f.infFn("Listening on %s %s", listenOn, f.conf.Listen)
	f.stopFn = func() {
		if err := srvStop(); err != nil {
			f.errFn("Err: %s", err)
		}
		if closable, ok :=  f.Storage.(io.Closer); ok {
			if err := closable.Close(); err != nil {
				f.errFn("Err: %s", err)
			}
		}
		f.OAuthStorage.Close()
	}

	exit := w.RegisterSignalHandlers(w.SignalHandlers{
		syscall.SIGHUP: func(_ chan int) {
			f.infFn("SIGHUP received, reloading configuration")
		},
		syscall.SIGINT: func(exit chan int) {
			f.infFn("SIGINT received, stopping")
			exit <- 0
		},
		syscall.SIGTERM: func(exit chan int) {
			f.infFn("SIGITERM received, force stopping")
			exit <- 0
		},
		syscall.SIGQUIT: func(exit chan int) {
			f.infFn("SIGQUIT received, force stopping with core-dump")
			exit <- 0
		},
	}).Exec(func() error {
		if err := srvRun(); err != nil{
			f.errFn("Error: %s", err)
			return err
		}
		var err error
		// Doesn't block if no connections, but will otherwise wait until the timeout deadline.
		go func(e error) {
			if err = srvStop(); err != nil {
				f.errFn("Error: %s", err)
			}
		}(err)
		return err
	})
	if exit == 0 {
		f.infFn("Shutting down")
	}
	return nil
}
