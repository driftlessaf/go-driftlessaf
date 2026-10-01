/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package githubreconciler

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
	jwt "github.com/golang-jwt/jwt/v4"
	"golang.org/x/oauth2"
)

// fakeTokenTTL is how long a minted installation token lives: GitHub's hour,
// so a test observes an uninstall through the 401 path rather than through
// tokens that happen to expire between calls.
const fakeTokenTTL = time.Hour

// fakeGitHub serves the GitHub endpoints an App-backed ClientCache touches,
// with GitHub's shapes:
//
//   - GET /orgs/{org}/installation and GET /users/{user}/installation: the
//     App's installation on that account, 404 when there is none;
//   - POST /app/installations/{id}/access_tokens: 201 with a fakeTokenTTL
//     token for a live installation, mintStatus (default 404, what GitHub
//     answers for a deleted installation) for any other ID;
//   - GET /repos/{owner}/{repo}: 200 when the bearer token was minted for an
//     installation that is still live, 401 Bad credentials otherwise.
//
// The lookup and mint endpoints require a valid App JWT signed by
// testAppKey, answering 401 otherwise, as GitHub does. Account logins are
// matched case-insensitively. State is read under mu at request time, so a
// test can uninstall or reinstall an owner between calls.
type fakeGitHub struct {
	mu sync.Mutex
	// orgs and users map a lower-cased account login to its installation ID.
	orgs, users map[string]int64
	// login, when set for a lower-cased owner, is the account login the
	// lookup answers with instead of the owner as asked.
	login map[string]string
	// mintStatus answers a mint for an installation that is not live.
	mintStatus int
	// lookupStatus, when set, answers both installation lookup endpoints.
	lookupStatus int
	// block, when set for an owner, parks its organization lookup until the
	// channel is closed or the request's context ends; entered receives the
	// owner as each parked request arrives. Make both inside a synctest
	// bubble when the test runs in one.
	block   map[string]chan struct{}
	entered chan string

	lookupCalls map[string]int // owner → calls to either lookup endpoint
	mintCalls   map[int64]int  // installation ID → mint calls
	tokens      map[string]int64
}

func newFakeGitHub(orgs map[string]int64) *fakeGitHub {
	return &fakeGitHub{
		orgs:        orgs,
		users:       make(map[string]int64),
		login:       make(map[string]string),
		mintStatus:  http.StatusNotFound,
		block:       make(map[string]chan struct{}),
		entered:     make(chan string, 16),
		lookupCalls: make(map[string]int),
		mintCalls:   make(map[int64]int),
		tokens:      make(map[string]int64),
	}
}

// liveLocked reports whether id is currently an installation. Callers hold mu.
func (f *fakeGitHub) liveLocked(id int64) bool {
	for _, accounts := range []map[string]int64{f.orgs, f.users} {
		for _, v := range accounts {
			if v == id {
				return true
			}
		}
	}
	return false
}

// setInstall installs the App on organization org as installation id,
// replacing any earlier installation: an uninstall followed by a reinstall.
func (f *fakeGitHub) setInstall(org string, id int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.orgs[strings.ToLower(org)] = id
}

func (f *fakeGitHub) lookups(owner string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lookupCalls[strings.ToLower(owner)]
}

func (f *fakeGitHub) totalLookups() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.lookupCalls {
		n += c
	}
	return n
}

// validAppJWT reports whether r carries a JWT signed with the test App key.
func validAppJWT(r *http.Request) bool {
	signed, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return false
	}
	var claims jwt.RegisteredClaims
	parsed, err := jwt.ParseWithClaims(signed, &claims, func(*jwt.Token) (any, error) { return testAppKey().Public(), nil })
	return err == nil && parsed.Valid
}

func (f *fakeGitHub) mints(id int64) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mintCalls[id]
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if (strings.HasSuffix(r.URL.Path, "/installation") || strings.HasSuffix(r.URL.Path, "/access_tokens")) && !validAppJWT(r) {
		http.Error(w, `{"message":"A JSON web token could not be decoded"}`, http.StatusUnauthorized)
		return
	}
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/orgs/") && strings.HasSuffix(r.URL.Path, "/installation"):
		f.lookup(w, r, strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/orgs/"), "/installation"), f.orgs, true)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/users/") && strings.HasSuffix(r.URL.Path, "/installation"):
		f.lookup(w, r, strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/users/"), "/installation"), f.users, false)
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/app/installations/") && strings.HasSuffix(r.URL.Path, "/access_tokens"):
		f.mint(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/"):
		f.getRepo(w, r)
	default:
		http.Error(w, "unexpected request "+r.Method+" "+r.URL.Path, http.StatusTeapot)
	}
}

func (f *fakeGitHub) lookup(w http.ResponseWriter, r *http.Request, owner string, accounts map[string]int64, blockable bool) {
	owner = strings.ToLower(owner)
	f.mu.Lock()
	f.lookupCalls[owner]++
	block := f.block[owner]
	f.mu.Unlock()
	if blockable && block != nil {
		f.entered <- owner
		select {
		case <-block:
		case <-r.Context().Done():
			return
		}
	}

	f.mu.Lock()
	status := f.lookupStatus
	id, ok := accounts[owner]
	login := cmp.Or(f.login[owner], owner)
	f.mu.Unlock()
	switch {
	case status != 0:
		http.Error(w, `{"message":"lookup failed"}`, status)
	case !ok:
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	default:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "account": map[string]any{"login": login}})
	}
}

func (f *fakeGitHub) mint(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/app/installations/"), "/access_tokens"), 10, 64)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mintCalls[id]++
	if !f.liveLocked(id) {
		http.Error(w, `{"message":"Not Found"}`, f.mintStatus)
		return
	}
	token := fmt.Sprintf("tok-%d-%d", id, f.mintCalls[id])
	f.tokens[token] = id
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"token":      token,
		"expires_at": time.Now().Add(fakeTokenTTL).UTC().Format(time.RFC3339),
	})
}

func (f *fakeGitHub) getRepo(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	f.mu.Lock()
	id, ok := f.tokens[token]
	live := ok && f.liveLocked(id)
	f.mu.Unlock()
	if !live {
		http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/repos/"), "/")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"name": parts[len(parts)-1], "full_name": strings.Join(parts, "/")})
}

// handlerTransport serves requests from an http.Handler in-process. It is
// for testing/synctest bubbles, where a goroutine parked on a socket is not
// durably blocked and so fake time would never advance; a handler parked on
// a channel is. It serves the same fakeGitHub the httptest tests do.
type handlerTransport struct{ h http.Handler }

func (t handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	t.h.ServeHTTP(rec, r)
	if err := r.Context().Err(); err != nil {
		return nil, err
	}
	resp := rec.Result()
	resp.Request = r
	return resp, nil
}

var testAppKey = sync.OnceValue(func() *rsa.PrivateKey {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return key
})

// newTestApp builds a real App — the production constructor past key
// loading — talking to baseURL over tr.
func newTestApp(t *testing.T, tr http.RoundTripper, baseURL string, opts ...AppOption) *App {
	t.Helper()
	app, err := newApp(210473, ghinstallation.NewRSASigner(jwt.SigningMethodRS256, testAppKey()), tr, baseURL, opts...)
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	return app
}

// newTestAppCache wires an App into a ClientCache the way AppMain does
// (newClientCacheFor), against a fakeGitHub served over httptest.
//
// The one seam is the vended clients' host: they are built for
// api.github.com, so the transport wrapper (the WithConditionalRequests
// hook) points their requests at the fake. Everything under it — the
// oauth2 transport, the token source, the mint — is the production stack.
func newTestAppCache(t *testing.T, fake *fakeGitHub) *ClientCache {
	t.Helper()
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	target, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parsing fake URL: %v", err)
	}
	app := newTestApp(t, srv.Client().Transport, srv.URL)
	return newClientCacheFor(mainOptions{
		tsff:          func(string) TokenSourceFunc { return app.TokenSourceFunc() },
		installIDFunc: app.LookupInstallID,
		wrapTransport: func(base http.RoundTripper) http.RoundTripper {
			return redirectTransport{base: base, target: target}
		},
	}, "test")
}

// redirectTransport sends every request to target's host.
type redirectTransport struct {
	base   http.RoundTripper
	target *url.URL
}

func (t redirectTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.URL.Scheme, r.URL.Host, r.Host = t.target.Scheme, t.target.Host, t.target.Host
	return t.base.RoundTrip(r)
}

// newInProcessAppCache wires an App into a ClientCache the way AppMain does,
// serving fake in-process for synctest bubbles. The returned ctx carries the
// in-process transport as oauth2.HTTPClient, which is where the vended
// clients take their base transport from, as oauth2.NewClient's do.
func newInProcessAppCache(t *testing.T, fake *fakeGitHub, opts ...AppOption) (context.Context, *ClientCache, *App) {
	t.Helper()
	tr := handlerTransport{fake}
	app := newTestApp(t, tr, "http://github.test", opts...)
	cc := newClientCacheFor(mainOptions{
		tsff:          func(string) TokenSourceFunc { return app.TokenSourceFunc() },
		installIDFunc: app.LookupInstallID,
	}, "test")
	return context.WithValue(t.Context(), oauth2.HTTPClient, &http.Client{Transport: tr}), cc, app
}
