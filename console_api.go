package main

import (
	"net/http"
	"net/url"
	"strings"
)

// Console requests use server-side credentials. Credentials never enter HTML or
// browser storage; external/device API routes retain their existing checks.
func consoleAPI(mux http.Handler, cfg Config) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("Origin") == "" && r.Header.Get("Sec-Fetch-Site") != "same-origin" {
			http.Error(w, "same-origin console request required", http.StatusForbidden)
			return
		}
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
			http.Error(w, "cross-origin console request denied", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || u.Host != r.Host || (u.Scheme != "http" && u.Scheme != "https") {
				http.Error(w, "cross-origin console request denied", http.StatusForbidden)
				return
			}
		}
		path := strings.TrimPrefix(r.URL.Path, "/console")
		var token string
		switch {
		case path == "/v1/ota/releases" || strings.HasPrefix(path, "/v1/ota/releases/"):
			token = cfg.OTA.AdminToken
		case path == "/v1/web/current" || path == "/v1/web/releases" || strings.HasPrefix(path, "/v1/web/releases/"):
			token = cfg.WebApp.AdminToken
		case path == "/v1/devices" || (strings.HasPrefix(path, "/v1/devices/") && strings.HasSuffix(path, "/speak")),
			path == "/v1/voice/records" || strings.HasPrefix(path, "/v1/voice/records/"):
			token = cfg.Voice.APIToken
		default:
			http.NotFound(w, r)
			return
		}
		if token == "" {
			http.Error(w, "service not configured", http.StatusServiceUnavailable)
			return
		}
		request := r.Clone(r.Context())
		request.URL.Path = path
		request.URL.RawPath = ""
		request.Header.Set("Authorization", "Bearer "+token)
		mux.ServeHTTP(w, request)
	})
}
