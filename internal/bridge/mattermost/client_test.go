package mattermost_test

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/bridge/mattermost"
)

const (
	testToken     = "xoxb-not-a-real-mattermost-token-9f21"
	testBotID     = "bot4k9x2mf7qytr3dnw8phe6suc"
	testBotTag    = "cairn"
	testChannelID = "chn1"
	testUserID    = "usr1"
	testPostText  = "hello"
	testPostID    = "p1"
)

// quietLog keeps the client's warnings out of the test output; the assertions
// read returned values, not log lines.
func quietLog() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// serveConnect answers the two calls Connect makes. maxPostSize is written
// through verbatim so a caller can hand back something unreadable.
func serveConnect(t *testing.T, mux *http.ServeMux, maxPostSize string) {
	t.Helper()

	mux.HandleFunc("/api/v4/users/me", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, mattermost.User{ID: testBotID, Username: testBotTag, IsBot: true})
	})

	mux.HandleFunc("/api/v4/config/client", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("format") != "old" {
			w.WriteHeader(http.StatusBadRequest)

			return
		}

		writeJSON(t, w, map[string]string{"MaxPostSize": maxPostSize})
	})
}

// writeJSON answers a request. A failure here means the stub server broke, not
// the client, so it fails the test rather than passing a truncated body on.
func writeJSON(t *testing.T, w http.ResponseWriter, body any) {
	t.Helper()

	w.Header().Set("Content-Type", "application/json")

	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Errorf("encode stub response: %v", err)
	}
}

// newTestClient points a client at server with the plain-http opt-in, which is
// the only way to reach an httptest server.
func newTestClient(t *testing.T, server *httptest.Server, limiter *mattermost.Bucket) *mattermost.Client {
	t.Helper()

	client, err := mattermost.NewClient(mattermost.ClientConfig{
		URL: server.URL, Token: testToken, InsecureHTTP: true, Log: quietLog(), Limiter: limiter,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	return client
}

// newConnectedClient returns a client that has already learned its identity.
func newConnectedClient(t *testing.T, mux *http.ServeMux) *mattermost.Client {
	t.Helper()

	serveConnect(t, mux, "16383")

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	client := newTestClient(t, server, nil)
	if err := client.Connect(t.Context()); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	return client
}

func TestNewClientValidatesTheServerURL(t *testing.T) {
	t.Parallel()

	cases := []struct {
		wantErr  error
		name     string
		url      string
		token    string
		insecure bool
	}{
		{name: "https", url: "https://mm.example.test", token: testToken},
		{name: "trailing slash", url: "https://mm.example.test/", token: testToken},
		{name: "plain http refused", url: "http://mm.example.test", token: testToken, wantErr: mattermost.ErrInsecureURL},
		{name: "plain http opted in", url: "http://mm.example.test", token: testToken, insecure: true},
		{name: "bare host", url: "mm.example.test", token: testToken, wantErr: mattermost.ErrBadURL},
		{name: "empty url", url: "", token: testToken, wantErr: mattermost.ErrNoURL},
		{name: "no host", url: "https://", token: testToken, wantErr: mattermost.ErrBadURL},
		{
			name: "query string", url: "https://mm.example.test?tenant=a",
			token: testToken, wantErr: mattermost.ErrBadURLQuery,
		},
		{
			name: "fragment", url: "https://mm.example.test#anchor",
			token: testToken, wantErr: mattermost.ErrBadURLQuery,
		},
		{name: "no token", url: "https://mm.example.test", token: "", wantErr: mattermost.ErrNoToken},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			_, err := mattermost.NewClient(mattermost.ClientConfig{
				URL: testCase.url, Token: testCase.token, InsecureHTTP: testCase.insecure,
			})

			if !errors.Is(err, testCase.wantErr) {
				t.Fatalf("NewClient(%q) error = %v, want %v", testCase.url, err, testCase.wantErr)
			}
		})
	}
}

func TestNewClientReportsAnUnparseableURL(t *testing.T) {
	t.Parallel()

	if _, err := mattermost.NewClient(mattermost.ClientConfig{URL: "://mm.example.test", Token: testToken}); err == nil {
		t.Fatal("NewClient accepted a url that does not parse")
	}
}

func TestConnectLearnsTheIdentityAndPostSize(t *testing.T) {
	t.Parallel()

	var gotAuth atomic.Value

	mux := http.NewServeMux()
	serveConnect(t, mux, "16383")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)

	client := newTestClient(t, server, nil)

	if err := client.Connect(t.Context()); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	me := client.Me()
	if me.ID != testBotID || me.Username != testBotTag || !me.IsBot {
		t.Fatalf("Me() = %+v, want the bot the server described", me)
	}

	if got := client.MaxPostSize(); got != 16383 {
		t.Fatalf("MaxPostSize() = %d, want the 16383 the server reported", got)
	}

	if auth, _ := gotAuth.Load().(string); auth != "Bearer "+testToken {
		t.Fatalf("Authorization header = %q, want the bearer token", auth)
	}
}

func TestConnectFallsBackWhenThePostSizeIsUnreadable(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		maxPostSize string
		status      int
	}{
		{name: "not a number", maxPostSize: "unbounded"},
		{name: "empty string", maxPostSize: ""},
		{name: "zero", maxPostSize: "0"},
		{name: "negative", maxPostSize: "-1"},
		{name: "config call fails", status: http.StatusInternalServerError},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			mux := http.NewServeMux()
			mux.HandleFunc("/api/v4/users/me", func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, mattermost.User{ID: testBotID, Username: testBotTag, IsBot: true})
			})
			mux.HandleFunc("/api/v4/config/client", func(w http.ResponseWriter, _ *http.Request) {
				if testCase.status != 0 {
					w.WriteHeader(testCase.status)

					return
				}

				writeJSON(t, w, map[string]string{"MaxPostSize": testCase.maxPostSize})
			})

			server := httptest.NewServer(mux)
			t.Cleanup(server.Close)

			client := newTestClient(t, server, nil)

			if err := client.Connect(t.Context()); err != nil {
				t.Fatalf("Connect: %v, want an unreadable post size to be survivable", err)
			}

			if got := client.MaxPostSize(); got != 4000 {
				t.Fatalf("MaxPostSize() = %d, want the 4000 fallback", got)
			}

			if client.Me().ID != testBotID {
				t.Fatal("Connect dropped the identity it had already read")
			}
		})
	}
}

func TestConnectReportsAFailedIdentityCall(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/users/me", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	client := newTestClient(t, server, nil)

	err := client.Connect(t.Context())
	if class := classOf(t, err); class != mattermost.ClassPermanent {
		t.Fatalf("Connect against a revoked token: class %v, want permanent", class)
	}
}

func TestCreatePostCarriesChannelRootAndMessage(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		rootID   string
		wantRoot bool
	}{
		{name: "root post", rootID: ""},
		{name: "thread reply", rootID: "pstroot4m9kx2wqz7tdn5cvbhp", wantRoot: true},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			var body atomic.Value

			mux := http.NewServeMux()
			mux.HandleFunc("/api/v4/posts", func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				body.Store(string(raw))

				w.WriteHeader(http.StatusCreated)
				writeJSON(t, w, map[string]any{"id": "pstnew", "channel_id": testChannelID, "root_id": testCase.rootID})
			})

			client := newConnectedClient(t, mux)

			post, err := client.CreatePost(t.Context(), testChannelID, testCase.rootID, "hello from the bridge")
			if err != nil {
				t.Fatalf("CreatePost: %v", err)
			}

			if post.ID != "pstnew" {
				t.Fatalf("CreatePost returned post %q, want the id the server assigned", post.ID)
			}

			sent := map[string]any{}
			raw, _ := body.Load().(string)

			if err := json.Unmarshal([]byte(raw), &sent); err != nil {
				t.Fatalf("request body %q does not parse: %v", raw, err)
			}

			if sent["channel_id"] != testChannelID || sent["message"] != "hello from the bridge" {
				t.Fatalf("request body = %v, want the channel and message as given", sent)
			}

			_, hasRoot := sent["root_id"]
			if hasRoot != testCase.wantRoot {
				t.Fatalf("root_id present = %v, want %v", hasRoot, testCase.wantRoot)
			}
		})
	}
}

func TestDirectChannelSendsABareUserIDArray(t *testing.T) {
	t.Parallel()

	var body atomic.Value

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/channels/direct", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body.Store(string(raw))

		writeJSON(t, w, mattermost.Channel{ID: "dmc1", Type: mattermost.ChannelDirect})
	})

	client := newConnectedClient(t, mux)

	channel, err := client.DirectChannel(t.Context(), "usr7h2qd4kmn9wxtc5bf3yjper")
	if err != nil {
		t.Fatalf("DirectChannel: %v", err)
	}

	if channel.ID != "dmc1" || channel.Type != mattermost.ChannelDirect {
		t.Fatalf("DirectChannel returned %+v, want the DM channel the server described", channel)
	}

	var ids []string

	raw, _ := body.Load().(string)
	if err := json.Unmarshal([]byte(raw), &ids); err != nil {
		t.Fatalf("request body %q is not a bare array: %v", raw, err)
	}

	want := []string{testBotID, "usr7h2qd4kmn9wxtc5bf3yjper"}
	if len(ids) != len(want) || ids[0] != want[0] || ids[1] != want[1] {
		t.Fatalf("request body = %v, want the two user ids in order", ids)
	}
}

func TestDirectChannelNeedsAConnectedIdentity(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.NewServeMux())
	t.Cleanup(server.Close)

	client := newTestClient(t, server, nil)

	_, err := client.DirectChannel(t.Context(), testUserID)
	if !errors.Is(err, mattermost.ErrNotConnected) {
		t.Fatalf("DirectChannel before Connect = %v, want ErrNotConnected", err)
	}
}

func TestUserByUsernameResolvesByName(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/users/username/alice", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, mattermost.User{ID: testUserID, Username: "alice"})
	})

	client := newConnectedClient(t, mux)

	user, err := client.UserByUsername(t.Context(), "alice")
	if err != nil {
		t.Fatalf("UserByUsername: %v", err)
	}

	if user.ID != testUserID {
		t.Fatalf("UserByUsername returned %+v, want %s", user, testUserID)
	}
}

func TestUserCachesTheBotFlag(t *testing.T) {
	t.Parallel()

	var hits atomic.Int64

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/users/"+testUserID, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		writeJSON(t, w, mattermost.User{ID: testUserID, Username: "ci-bot", IsBot: true})
	})

	client := newConnectedClient(t, mux)

	for call := range 3 {
		user, err := client.User(t.Context(), testUserID)
		if err != nil {
			t.Fatalf("User call %d: %v", call+1, err)
		}

		if !user.IsBot {
			t.Fatalf("User call %d returned %+v, want is_bot set", call+1, user)
		}
	}

	if got := hits.Load(); got != 1 {
		t.Fatalf("the server saw %d lookups, want one: is_bot is cached", got)
	}
}

func TestChannelByNameUsesTeamAndChannelName(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/teams/name/ops/channels/name/alerts", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, mattermost.Channel{ID: testChannelID, Name: "alerts", Type: mattermost.ChannelOpen, TeamID: "tm1"})
	})

	client := newConnectedClient(t, mux)

	channel, err := client.ChannelByName(t.Context(), "ops", "alerts")
	if err != nil {
		t.Fatalf("ChannelByName: %v", err)
	}

	if channel.ID != testChannelID || channel.Name != "alerts" {
		t.Fatalf("ChannelByName returned %+v, want the alerts channel", channel)
	}
}

func TestTypingPostsAsTheIdentity(t *testing.T) {
	t.Parallel()

	var body atomic.Value

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/users/"+testBotID+"/typing", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body.Store(string(raw))

		w.WriteHeader(http.StatusOK)
	})

	client := newConnectedClient(t, mux)

	if err := client.Typing(t.Context(), testChannelID, "pstroot"); err != nil {
		t.Fatalf("Typing: %v", err)
	}

	sent := map[string]string{}
	raw, _ := body.Load().(string)

	if err := json.Unmarshal([]byte(raw), &sent); err != nil {
		t.Fatalf("request body %q does not parse: %v", raw, err)
	}

	if sent["channel_id"] != testChannelID || sent["parent_id"] != "pstroot" {
		t.Fatalf("request body = %v, want the channel and thread the turn runs in", sent)
	}
}

func TestErrorClassification(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		reset     string
		status    int
		wantClass mattermost.ErrorClass
		wantRetry time.Duration
	}{
		{name: "revoked token", status: http.StatusUnauthorized, wantClass: mattermost.ClassPermanent},
		{name: "missing permission", status: http.StatusForbidden, wantClass: mattermost.ClassPermanent},
		{name: "bad request", status: http.StatusBadRequest, wantClass: mattermost.ClassPermanent},
		{name: "unknown channel", status: http.StatusNotFound, wantClass: mattermost.ClassNotFound},
		{name: "rate limited", status: http.StatusTooManyRequests, reset: "3", wantClass: mattermost.ClassRateLimited, wantRetry: 3 * time.Second},
		// A 429 the server did not put a reset window on still parks for a second.
		// Zero would let the retry spend the next token straight back into the
		// limit it just hit.
		{
			name: "rate limited without a hint", status: http.StatusTooManyRequests,
			wantClass: mattermost.ClassRateLimited, wantRetry: time.Second,
		},
		{
			name: "rate limited with junk", status: http.StatusTooManyRequests, reset: "soon",
			wantClass: mattermost.ClassRateLimited, wantRetry: time.Second,
		},
		{name: "rate limited beyond the cap", status: http.StatusTooManyRequests, reset: "86400", wantClass: mattermost.ClassRateLimited, wantRetry: time.Minute},
		{name: "server error", status: http.StatusInternalServerError, wantClass: mattermost.ClassTransient},
		{name: "gateway timeout", status: http.StatusBadGateway, wantClass: mattermost.ClassTransient},
		{name: "request timeout", status: http.StatusRequestTimeout, wantClass: mattermost.ClassTransient},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			mux := http.NewServeMux()
			mux.HandleFunc("/api/v4/posts", func(w http.ResponseWriter, _ *http.Request) {
				if testCase.reset != "" {
					w.Header().Set("X-Ratelimit-Reset", testCase.reset)
				}

				w.WriteHeader(testCase.status)
				_, _ = w.Write([]byte("limit exceeded"))
			})

			client := newConnectedClient(t, mux)

			_, err := client.CreatePost(t.Context(), testChannelID, "", testPostText)

			failure := apiError(t, err)
			if failure.Class != testCase.wantClass {
				t.Fatalf("status %d gave class %v, want %v", testCase.status, failure.Class, testCase.wantClass)
			}

			if failure.Status != testCase.status {
				t.Fatalf("error carried status %d, want %d", failure.Status, testCase.status)
			}

			if failure.RetryAfter != testCase.wantRetry {
				t.Fatalf("error carried RetryAfter %v, want %v", failure.RetryAfter, testCase.wantRetry)
			}

			if failure.Method != "CreatePost" {
				t.Fatalf("error names method %q, want the failing call", failure.Method)
			}
		})
	}
}

func TestErrorsNeverCarryTheToken(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/posts", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})

	client := newConnectedClient(t, mux)

	_, err := client.CreatePost(t.Context(), testChannelID, "", testPostText)

	failure := apiError(t, err)

	rendered := failure.Error()
	if strings.Contains(rendered, testToken) || strings.Contains(rendered, "Authorization") {
		t.Fatalf("error %q leaks the credential; the token rides a header and must stay there", rendered)
	}

	if failure.Message == "" || failure.Method == "" {
		t.Fatalf("error %+v says nothing about which call failed", failure)
	}
}

func TestRateLimitedResponseParksTheSharedBucket(t *testing.T) {
	t.Parallel()

	clock := newTestClock()
	bucket := mattermost.NewBucket(mattermost.BucketConfig{
		Now: clock.Now, Sleep: clock.Sleep, RatePerSecond: 100, Burst: 100,
	})

	mux := http.NewServeMux()
	serveConnect(t, mux, "16383")
	mux.HandleFunc("/api/v4/posts", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Ratelimit-Reset", "4")
		w.WriteHeader(http.StatusTooManyRequests)
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	client := newTestClient(t, server, bucket)
	if err := client.Connect(t.Context()); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	if _, err := client.CreatePost(t.Context(), testChannelID, "", testPostText); err == nil {
		t.Fatal("CreatePost against a 429 returned no error")
	}

	if err := bucket.Wait(t.Context()); err != nil {
		t.Fatalf("Wait after the park: %v", err)
	}

	waits := clock.waits()
	if len(waits) != 1 || waits[0] != 4*time.Second {
		t.Fatalf("bucket waited %v, want the four seconds the server asked for", waits)
	}
}

// apiError asserts that err is a classified Mattermost failure and returns it.
func apiError(t *testing.T, err error) *mattermost.APIError {
	t.Helper()

	failure, ok := errors.AsType[*mattermost.APIError](err)
	if !ok {
		t.Fatalf("error %v is not a *mattermost.APIError", err)
	}

	return failure
}

// classOf returns the class of a classified failure.
func classOf(t *testing.T, err error) mattermost.ErrorClass {
	t.Helper()

	return apiError(t, err).Class
}

// TestCreatePostRefusesARedirect proves a 3xx reaches the status classifier
// instead of being followed. Go re-issues a redirected POST as a GET with the
// body dropped, so following one answers 200 with a post that has no id: the
// reply never reaches the channel and the turn records ok.
func TestCreatePostRefusesARedirect(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()

	mux.HandleFunc("/api/v4/posts", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "/api/v4/posts/")
		w.WriteHeader(http.StatusMovedPermanently)
	})

	mux.HandleFunc("/api/v4/posts/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, mattermost.Post{ChannelID: testChannelID})
	})

	client := newConnectedClient(t, mux)

	post, err := client.CreatePost(t.Context(), testChannelID, "", testPostText)
	if err == nil {
		t.Fatalf("CreatePost followed the redirect and reported success: %+v", post)
	}

	failure, ok := errors.AsType[*mattermost.APIError](err)
	if !ok || failure.Status != http.StatusMovedPermanently {
		t.Fatalf("CreatePost error = %v, want the 301 classified as a failure", err)
	}
}

// TestCreatePostRetriesAfterARateLimit proves a 429 is retried inside the call's
// own deadline rather than dropping the reply outright (design section 5, edge
// case 11).
func TestCreatePostRetriesAfterARateLimit(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32

	mux := http.NewServeMux()

	mux.HandleFunc("/api/v4/posts", func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) == 1 {
			w.Header().Set("X-Ratelimit-Reset", "1")
			w.WriteHeader(http.StatusTooManyRequests)

			return
		}

		writeJSON(t, w, mattermost.Post{ID: testPostID, ChannelID: testChannelID})
	})

	client := newConnectedClient(t, mux)

	post, err := client.CreatePost(t.Context(), testChannelID, "", testPostText)
	if err != nil {
		t.Fatalf("CreatePost after a 429: %v", err)
	}

	if post.ID != testPostID {
		t.Fatalf("CreatePost returned %+v, want the post the retry created", post)
	}

	if got := attempts.Load(); got != 2 {
		t.Fatalf("the server saw %d attempts, want the 429 and one retry", got)
	}
}
