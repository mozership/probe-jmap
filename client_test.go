package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/linyows/probe/actionrpc"
)

// jmapServer serves a session at /.well-known/jmap that names /api/ as its
// API URL, and answers the API with api. It records the requests it got.
type jmapServer struct {
	*httptest.Server
	sessionCode int
	// primaryAccounts is what the session names as its primary accounts.
	primaryAccounts map[string]any
	api             func(w http.ResponseWriter, calls []any)
	// sessions and posts are the requests the session and the API got.
	sessions []*http.Request
	posts    []map[string]any
}

func newJMAPServer(t *testing.T, api func(w http.ResponseWriter, calls []any)) *jmapServer {
	t.Helper()
	s := &jmapServer{sessionCode: http.StatusOK, api: api, primaryAccounts: map[string]any{
		"urn:ietf:params:jmap:mail":       "mail-account",
		"urn:ietf:params:jmap:submission": "submission-account",
	}}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/jmap", func(w http.ResponseWriter, r *http.Request) {
		s.sessions = append(s.sessions, r.Clone(r.Context()))
		if s.sessionCode != http.StatusOK {
			w.WriteHeader(s.sessionCode)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"capabilities":    map[string]any{coreCapability: map[string]any{}},
			"primaryAccounts": s.primaryAccounts,
			"apiUrl":          "/api/",
		})
	})
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		body["_header"] = r.Header.Clone()
		s.posts = append(s.posts, body)
		calls, _ := body["methodCalls"].([]any)
		s.api(w, calls)
	})
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

// echo answers each call with its own name and arguments.
func echo(w http.ResponseWriter, calls []any) {
	writeResponses(w, calls)
}

func writeResponses(w http.ResponseWriter, responses []any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"methodResponses": responses, "sessionState": "s1"})
}

func TestRun(t *testing.T) {
	s := newJMAPServer(t, echo)

	ret, err := (&Action{}).Run(map[string]any{
		"url":        s.URL,
		"basic_auth": map[string]any{"username": "alice", "password": "secret"},
		"calls": []any{
			map[string]any{"method": "Email/query", "id": "q", "args": map[string]any{"limit": 1}},
			map[string]any{"method": "Email/get", "args": map[string]any{
				"#ids": map[string]any{"resultOf": "q", "path": "/ids"},
			}},
			map[string]any{"method": "Identity/get"},
			map[string]any{"method": "Mailbox/get", "args": map[string]any{"accountId": "other"}},
			map[string]any{"method": "Core/echo", "args": map[string]any{"hello": true}},
		},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if len(s.sessions) != 1 || len(s.posts) != 1 {
		t.Fatalf("got %d session and %d API requests, want 1 each", len(s.sessions), len(s.posts))
	}
	for _, h := range []http.Header{s.sessions[0].Header, s.posts[0]["_header"].(http.Header)} {
		if got := h.Get("Authorization"); got != "Basic YWxpY2U6c2VjcmV0" {
			t.Errorf("Authorization = %q", got)
		}
		if got := h.Get("User-Agent"); got != "probe-jmap/dev" {
			t.Errorf("User-Agent = %q", got)
		}
	}
	post := s.posts[0]
	if got := post["_header"].(http.Header).Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
	wantUsing := []any{coreCapability, "urn:ietf:params:jmap:mail", "urn:ietf:params:jmap:submission"}
	if !reflect.DeepEqual(post["using"], wantUsing) {
		t.Errorf("using = %v, want %v", post["using"], wantUsing)
	}
	calls := post["methodCalls"].([]any)
	accounts := []any{}
	for _, c := range calls {
		accounts = append(accounts, c.([]any)[1].(map[string]any)["accountId"])
	}
	if want := []any{"mail-account", "mail-account", "submission-account", "other", nil}; !reflect.DeepEqual(accounts, want) {
		t.Errorf("accountIds = %v, want %v", accounts, want)
	}
	ref := calls[1].([]any)[1].(map[string]any)["#ids"].(map[string]any)
	if ref["name"] != "Email/query" || calls[1].([]any)[2] != "c1" {
		t.Errorf("Email/get call = %v", calls[1])
	}

	if ret["status"] != 0 {
		t.Errorf("status = %v, want 0", ret["status"])
	}
	req := ret["req"].(map[string]any)
	if req["url"] != s.URL+"/api/" || req["session_url"] != s.URL+"/.well-known/jmap" {
		t.Errorf("req.url = %v, req.session_url = %v", req["url"], req["session_url"])
	}
	res := ret["res"].(map[string]any)
	if res["code"] != 200 {
		t.Errorf("res.code = %v", res["code"])
	}
	if res["session"].(map[string]any)["apiUrl"] != "/api/" {
		t.Errorf("res.session = %v", res["session"])
	}
	responses := res["responses"].([]any)
	if len(responses) != 5 || responses[0].(map[string]any)["name"] != "Email/query" || responses[0].(map[string]any)["id"] != "q" {
		t.Errorf("res.responses = %v", responses)
	}
	if limit := res["results"].(map[string]any)["q"].(map[string]any)["limit"]; limit != float64(1) {
		t.Errorf("res.results.q.limit = %v", limit)
	}
	if errs := res["errors"].([]any); len(errs) != 0 {
		t.Errorf("res.errors = %v, want none", errs)
	}
	if _, err := time.ParseDuration(ret["rt"].(string)); err != nil {
		t.Errorf("rt = %v: %v", ret["rt"], err)
	}
	// The result goes to probe over gRPC, which takes only the types it has.
	if _, err := actionrpc.Sendable(ret); err != nil {
		t.Errorf("result cannot be sent to probe: %v", err)
	}
}

// A push subscription is tied to no account, so its methods are given no
// accountId, not even the one with.account_id names.
func TestRunGivesAccountlessMethodsNoAccount(t *testing.T) {
	s := newJMAPServer(t, echo)

	ret, err := (&Action{}).Run(map[string]any{
		"url":        s.URL,
		"account_id": "a1",
		"calls": []any{
			map[string]any{"method": "PushSubscription/get"},
			map[string]any{"method": "Core/echo"},
			map[string]any{"method": "Email/get"},
			map[string]any{"method": "Blob/copy"},
		},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if ret["status"] != 0 {
		t.Errorf("status = %v, want 0", ret["status"])
	}
	post := s.posts[0]
	if want := []any{coreCapability, "urn:ietf:params:jmap:mail"}; !reflect.DeepEqual(post["using"], want) {
		t.Errorf("using = %v, want %v", post["using"], want)
	}
	accounts := []any{}
	for _, c := range post["methodCalls"].([]any) {
		accounts = append(accounts, c.([]any)[1].(map[string]any)["accountId"])
	}
	if want := []any{nil, nil, "a1", "a1"}; !reflect.DeepEqual(accounts, want) {
		t.Errorf("accountIds = %v, want %v", accounts, want)
	}
}

// A session may name a primary account for the core capability. Blob/copy,
// a method of that capability, is made in it, not in the one of the blob
// capability, and a push subscription is still made in none.
func TestRunPrimaryAccountOfCore(t *testing.T) {
	s := newJMAPServer(t, echo)
	s.primaryAccounts = map[string]any{
		coreCapability:              "core-account",
		"urn:ietf:params:jmap:blob": "blob-account",
	}

	_, err := (&Action{}).Run(map[string]any{
		"url": s.URL,
		"calls": []any{
			map[string]any{"method": "Blob/copy"},
			map[string]any{"method": "Blob/get"},
			map[string]any{"method": "PushSubscription/get"},
		},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	accounts := []any{}
	for _, c := range s.posts[0]["methodCalls"].([]any) {
		accounts = append(accounts, c.([]any)[1].(map[string]any)["accountId"])
	}
	if want := []any{"core-account", "blob-account", nil}; !reflect.DeepEqual(accounts, want) {
		t.Errorf("accountIds = %v, want %v", accounts, want)
	}
}

// The session is found by a redirect from the well-known URI, as many
// servers answer it with one.
func TestRunFollowsRedirectToSession(t *testing.T) {
	s := newJMAPServer(t, echo)
	front := httptest.NewServer(http.RedirectHandler(s.URL+"/.well-known/jmap", http.StatusTemporaryRedirect))
	t.Cleanup(front.Close)

	ret, err := (&Action{}).Run(map[string]any{"url": front.URL, "calls": []any{map[string]any{"method": "Core/echo"}}})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if ret["status"] != 0 || ret["req"].(map[string]any)["url"] != s.URL+"/api/" {
		t.Errorf("status = %v, req.url = %v", ret["status"], ret["req"].(map[string]any)["url"])
	}
}

// A response is a result even when it reports a failure, so that a test can
// assert on it, and status tells it failed.
func TestRunFailedResponses(t *testing.T) {
	tests := []struct {
		name  string
		api   func(w http.ResponseWriter, calls []any)
		check func(t *testing.T, res map[string]any)
	}{
		{
			name: "method error",
			api: func(w http.ResponseWriter, calls []any) {
				writeResponses(w, []any{[]any{"error", map[string]any{"type": "unknownMethod"}, "c0"}})
			},
			check: func(t *testing.T, res map[string]any) {
				want := []any{map[string]any{"type": "unknownMethod", "kind": "method", "id": "c0", "method": "Nope/get"}}
				if !reflect.DeepEqual(res["errors"], want) {
					t.Errorf("res.errors = %v, want %v", res["errors"], want)
				}
				if res["results"].(map[string]any)["c0"].(map[string]any)["type"] != "unknownMethod" {
					t.Errorf("res.results = %v", res["results"])
				}
			},
		},
		{
			name: "set error",
			api: func(w http.ResponseWriter, calls []any) {
				writeResponses(w, []any{[]any{"Nope/get", map[string]any{
					"created":    map[string]any{"a": map[string]any{"id": "1"}},
					"notCreated": map[string]any{"b": map[string]any{"type": "invalidProperties", "properties": []any{"to"}}},
				}, "c0"}})
			},
			check: func(t *testing.T, res map[string]any) {
				want := []any{map[string]any{"type": "invalidProperties", "properties": []any{"to"}, "kind": "notCreated", "id": "c0", "method": "Nope/get", "key": "b"}}
				if !reflect.DeepEqual(res["errors"], want) {
					t.Errorf("res.errors = %v, want %v", res["errors"], want)
				}
			},
		},
		{
			name: "request error",
			api: func(w http.ResponseWriter, calls []any) {
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"type":"urn:ietf:params:jmap:error:unknownCapability","status":400}`)
			},
			check: func(t *testing.T, res map[string]any) {
				errs := res["errors"].([]any)
				if res["code"] != 400 || len(errs) != 1 || errs[0].(map[string]any)["kind"] != "request" ||
					errs[0].(map[string]any)["type"] != "urn:ietf:params:jmap:error:unknownCapability" {
					t.Errorf("res.code = %v, res.errors = %v", res["code"], errs)
				}
			},
		},
		{
			name: "not json",
			api: func(w http.ResponseWriter, calls []any) {
				_, _ = io.WriteString(w, "oops")
			},
			check: func(t *testing.T, res map[string]any) {
				if res["body"] != "oops" {
					t.Errorf("res.body = %v", res["body"])
				}
				if _, ok := res["rawbody"]; ok {
					t.Error("res.rawbody is set for a body that is not JSON")
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newJMAPServer(t, tt.api)
			ret, err := (&Action{}).Run(map[string]any{
				"url":   s.URL,
				"using": []any{coreCapability},
				"calls": []any{map[string]any{"method": "Nope/get"}},
			})
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if ret["status"] != 1 {
				t.Errorf("status = %v, want 1", ret["status"])
			}
			tt.check(t, ret["res"].(map[string]any))
		})
	}
}

// A session the server does not give is a result, and the methods are not
// sent.
func TestRunWithoutSession(t *testing.T) {
	s := newJMAPServer(t, echo)
	s.sessionCode = http.StatusUnauthorized

	ret, err := (&Action{}).Run(map[string]any{"url": s.URL, "calls": []any{map[string]any{"method": "Core/echo"}}})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(s.posts) != 0 {
		t.Errorf("the API got %d requests, want none", len(s.posts))
	}
	res := ret["res"].(map[string]any)
	errs := res["errors"].([]any)
	if ret["status"] != 1 || res["code"] != 401 || res["session"] != nil || len(errs) != 1 ||
		errs[0].(map[string]any)["kind"] != "session" || !strings.Contains(errs[0].(map[string]any)["description"].(string), "401") {
		t.Errorf("status = %v, res = %v", ret["status"], res)
	}
	if _, err := actionrpc.Sendable(ret); err != nil {
		t.Errorf("result cannot be sent to probe: %v", err)
	}
}

// Only a request that gets no response is an error.
func TestRunNoResponse(t *testing.T) {
	s := newJMAPServer(t, echo)
	s.Close()
	if _, err := (&Action{}).Run(map[string]any{"url": s.URL, "calls": []any{map[string]any{"method": "Core/echo"}}}); err == nil {
		t.Error("Run() error = nil for a server that is down")
	}
}

func TestRunTimeout(t *testing.T) {
	s := newJMAPServer(t, func(w http.ResponseWriter, calls []any) {
		time.Sleep(200 * time.Millisecond)
		echo(w, calls)
	})
	_, err := (&Action{}).Run(map[string]any{"url": s.URL, "timeout": "50ms", "calls": []any{map[string]any{"method": "Core/echo"}}})
	if err == nil {
		t.Error("Run() error = nil for a server slower than the timeout")
	}
}

func TestRunStepGuard(t *testing.T) {
	s := newJMAPServer(t, echo)
	host := strings.TrimPrefix(s.URL, "http://")
	write := []any{map[string]any{"method": "Email/set"}}
	read := []any{map[string]any{"method": "Email/get"}}

	t.Run("read-only refuses a write before sending anything", func(t *testing.T) {
		_, _, err := (&Action{}).RunStep(actionrpc.Call{
			With:  map[string]any{"url": s.URL, "calls": write},
			Guard: actionrpc.Guard{ReadOnly: true},
		})
		if !actionrpc.IsRefused(err) {
			t.Errorf("RunStep() error = %v, want a refusal", err)
		}
		if len(s.sessions) != 0 {
			t.Errorf("the session got %d requests, want none", len(s.sessions))
		}
	})

	t.Run("read-only sends reads", func(t *testing.T) {
		ret, _, err := (&Action{}).RunStep(actionrpc.Call{
			With:  map[string]any{"url": s.URL, "calls": read},
			Guard: actionrpc.Guard{ReadOnly: true, AllowHosts: []string{host}},
		})
		if err != nil || ret["status"] != 0 {
			t.Errorf("RunStep() = %v, %v", ret, err)
		}
	})

	t.Run("a host the run does not allow", func(t *testing.T) {
		_, _, err := (&Action{}).RunStep(actionrpc.Call{
			With:  map[string]any{"url": s.URL, "calls": read},
			Guard: actionrpc.Guard{AllowHosts: []string{"jmap.example.com"}},
		})
		if !actionrpc.IsRefused(err) {
			t.Errorf("RunStep() error = %v, want a refusal", err)
		}
	})

	t.Run("a redirect to a host the run does not allow", func(t *testing.T) {
		front := httptest.NewServer(http.RedirectHandler(s.URL+"/.well-known/jmap", http.StatusTemporaryRedirect))
		t.Cleanup(front.Close)
		u, _ := url.Parse(front.URL)
		_, _, err := (&Action{}).RunStep(actionrpc.Call{
			With:  map[string]any{"url": front.URL, "calls": read},
			Guard: actionrpc.Guard{AllowHosts: []string{u.Host}},
		})
		if !actionrpc.IsRefused(err) {
			t.Errorf("RunStep() error = %v, want a refusal", err)
		}
	})
}
