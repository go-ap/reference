package app

import (
	pub "github.com/go-ap/activitypub"
	"github.com/go-ap/errors"
	"github.com/go-chi/chi"
	"github.com/go-chi/chi/middleware"
	"github.com/openshift/osin"
	"github.com/sirupsen/logrus"
	"net/http"
)

func (f FedBOX) CollectionRoutes(descend bool) func(chi.Router) {
	return func(r chi.Router) {
		r.Group(func(r chi.Router) {
			r.Method(http.MethodGet, "/", HandleCollection(f))
			r.Method(http.MethodHead, "/", HandleCollection(f))
			r.Method(http.MethodPost, "/", HandleRequest(f))

			r.Route("/{id}", func(r chi.Router) {
				r.Method(http.MethodGet, "/", HandleItem(f))
				r.Method(http.MethodHead, "/", HandleItem(f))
				if descend {
					r.Route("/{collection}", f.CollectionRoutes(false))
				}
			})
		})
	}
}

func (f FedBOX) Routes(baseURL string, os *osin.Server, l logrus.FieldLogger) func(chi.Router) {
	return func(r chi.Router) {
		r.Use(middleware.RealIP)
		r.Use(CleanRequestPath)
		r.Use(ActorFromAuthHeader(os, f.Storage, l))

		r.Method(http.MethodGet, "/", HandleItem(f))
		r.Method(http.MethodHead, "/", HandleItem(f))
		r.Route("/{collection}", f.CollectionRoutes(true))

		baseIRI := pub.IRI(baseURL)
		ia := indieAuth{
			baseIRI: baseIRI,
			genID:   GenerateID(baseIRI),
			os:      os,
			ap:      f.Storage,
		}
		if oauthStorage, ok := f.OAuthStorage.(ClientStorage); ok {
			ia.st = oauthStorage
		}
		h := oauthHandler{
			baseURL: baseURL,
			ia:      &ia,
			loader:  f.Storage,
			logger:  l,
		}
		r.Route("/oauth", func(r chi.Router) {
			// Authorization code endpoint
			r.Get("/authorize", h.Authorize)
			r.Post("/authorize", h.Authorize)
			// Access token endpoint
			r.Post("/token", h.Token)

			r.Group(func(r chi.Router) {
				r.Get("/login", h.ShowLogin)
				r.Post("/login", h.HandleLogin)
				r.Get("/pw", h.ShowChangePw)
				r.Post("/pw", h.HandleChangePw)
			})
		})

		notFound := errors.HandleError(errors.NotFoundf("invalid url"))
		r.Handle("/favicon.ico", notFound)
		r.NotFound(notFound.ServeHTTP)
		r.MethodNotAllowed(errors.HandleError(errors.MethodNotAllowedf("method not allowed")).ServeHTTP)
	}
}
