package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/linyows/probe/actionref"
	"github.com/linyows/probe/actionrpc"
)

func TestParseRequest(t *testing.T) {
	r, err := parseRequest(map[string]any{
		"url": "https://jmap.example.com",
		"calls": []any{
			map[string]any{"method": "Email/query", "args": map[string]any{"limit": 1}, "id": "q"},
			map[string]any{"method": "Email/get", "args": map[string]any{
				"#ids": map[string]any{"resultOf": "q", "path": "/ids"},
			}},
			map[string]any{"method": "Identity/get"},
		},
		"account_id": "a1",
		"headers":    map[string]any{"X-Trace": 1},
		"basic_auth": map[string]any{"username": "alice", "password": "secret"},
		"timeout":    "5s",
	})
	if err != nil {
		t.Fatalf("parseRequest() error = %v", err)
	}
	if r.sessionURL != "https://jmap.example.com/.well-known/jmap" {
		t.Errorf("sessionURL = %q", r.sessionURL)
	}
	wantUsing := []string{coreCapability, "urn:ietf:params:jmap:mail", "urn:ietf:params:jmap:submission"}
	if !reflect.DeepEqual(r.using, wantUsing) {
		t.Errorf("using = %v, want %v", r.using, wantUsing)
	}
	if ids := []string{r.calls[0].id, r.calls[1].id, r.calls[2].id}; !reflect.DeepEqual(ids, []string{"q", "c1", "c2"}) {
		t.Errorf("ids = %v", ids)
	}
	ref := r.calls[1].args["#ids"].(map[string]any)
	if ref["name"] != "Email/query" {
		t.Errorf("back-reference name = %v, want Email/query", ref["name"])
	}
	if r.calls[2].args == nil {
		t.Error("args of a call without args is nil, want an empty object")
	}
	if r.accountID != "a1" || r.headers["X-Trace"] != "1" || r.timeout != 5*time.Second {
		t.Errorf("accountID = %q, headers = %v, timeout = %v", r.accountID, r.headers, r.timeout)
	}
	if got := r.headers["Authorization"]; got != "Basic YWxpY2U6c2VjcmV0" {
		t.Errorf("Authorization = %q", got)
	}
}

func TestParseRequestSessionURL(t *testing.T) {
	calls := []any{map[string]any{"method": "Core/echo"}}
	for in, want := range map[string]string{
		"http://localhost:8080":                "http://localhost:8080/.well-known/jmap",
		"http://localhost:8080/":               "http://localhost:8080/.well-known/jmap",
		"https://api.example.com/jmap/session": "https://api.example.com/jmap/session",
	} {
		r, err := parseRequest(map[string]any{"url": in, "calls": calls})
		if err != nil {
			t.Fatalf("parseRequest(%q) error = %v", in, err)
		}
		if r.sessionURL != want {
			t.Errorf("sessionURL of %q = %q, want %q", in, r.sessionURL, want)
		}
	}
}

func TestParseRequestKeepsGivenUsingAndName(t *testing.T) {
	r, err := parseRequest(map[string]any{
		"url":   "http://localhost",
		"using": []any{"urn:example:x"},
		"calls": []any{
			map[string]any{"method": "X/query", "id": "q"},
			map[string]any{"method": "X/get", "args": map[string]any{
				"#ids": map[string]any{"resultOf": "q", "name": "X/query", "path": "/ids"},
			}},
		},
	})
	if err != nil {
		t.Fatalf("parseRequest() error = %v", err)
	}
	if !reflect.DeepEqual(r.using, []string{"urn:example:x"}) {
		t.Errorf("using = %v", r.using)
	}
}

func TestParseRequestErrors(t *testing.T) {
	echo := []any{map[string]any{"method": "Core/echo"}}
	tests := []struct {
		name string
		with map[string]any
		want string
	}{
		{"no url", map[string]any{"calls": echo}, "requires with.url"},
		{"not http", map[string]any{"url": "ftp://x", "calls": echo}, "http or https URL"},
		{"no calls", map[string]any{"url": "http://x"}, "requires with.calls"},
		{"empty calls", map[string]any{"url": "http://x", "calls": []any{}}, "requires with.calls"},
		{"call not object", map[string]any{"url": "http://x", "calls": []any{"Email/get"}}, "calls[0] must be an object"},
		{"no method", map[string]any{"url": "http://x", "calls": []any{map[string]any{}}}, "calls[0].method is required"},
		{"method without type", map[string]any{"url": "http://x", "calls": []any{map[string]any{"method": "get"}}}, "must be a type and a method"},
		{"unknown key", map[string]any{"url": "http://x", "calls": []any{map[string]any{"method": "Core/echo", "arguments": map[string]any{}}}}, "not arguments"},
		{"args not object", map[string]any{"url": "http://x", "calls": []any{map[string]any{"method": "Core/echo", "args": "x"}}}, "args must be an object"},
		{"duplicate id", map[string]any{"url": "http://x", "calls": []any{
			map[string]any{"method": "Core/echo", "id": "a"},
			map[string]any{"method": "Core/echo", "id": "a"},
		}}, "id of an earlier call"},
		{"unknown capability", map[string]any{"url": "http://x", "calls": []any{map[string]any{"method": "Calendar/get"}}}, "with.using is needed for Calendar/get"},
		{"using not list", map[string]any{"url": "http://x", "calls": echo, "using": "urn:x"}, "with.using must be a list"},
		{"auth and header", map[string]any{"url": "http://x", "calls": echo,
			"basic_auth": map[string]any{"username": "a", "password": "b"},
			"headers":    map[string]any{"authorization": "Bearer t"},
		}, "cannot be given together"},
		{"auth without username", map[string]any{"url": "http://x", "calls": echo, "basic_auth": map[string]any{"password": "b"}}, "username is required"},
		{"bad timeout", map[string]any{"url": "http://x", "calls": echo, "timeout": "soon"}, "with.timeout"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseRequest(tt.with)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("parseRequest() error = %v, want one containing %q", err, tt.want)
			}
		})
	}
}

func TestCheckReadOnly(t *testing.T) {
	reads := []methodCall{{method: "Email/query"}, {method: "Email/get"}, {method: "Mailbox/changes"}, {method: "Core/echo"}}
	if err := checkReadOnly(actionrpc.Guard{ReadOnly: true}, reads); err != nil {
		t.Errorf("checkReadOnly(reads) error = %v", err)
	}
	writes := append(reads, methodCall{method: "Email/set"})
	err := checkReadOnly(actionrpc.Guard{ReadOnly: true}, writes)
	if !actionrpc.IsRefused(err) || !strings.Contains(err.Error(), "Email/set may write") {
		t.Errorf("checkReadOnly(writes) error = %v, want a refusal of Email/set", err)
	}
	if err := checkReadOnly(actionrpc.Guard{}, writes); err != nil {
		t.Errorf("checkReadOnly() without a guard error = %v", err)
	}
}

func TestParseTimeout(t *testing.T) {
	for in, want := range map[any]time.Duration{"1m30s": 90 * time.Second, 2: 2 * time.Second, 0.5: 500 * time.Millisecond, int64(0): 0} {
		got, err := parseTimeout(in)
		if err != nil || got != want {
			t.Errorf("parseTimeout(%v) = %v, %v, want %v", in, got, err, want)
		}
	}
	if _, err := parseTimeout(true); err == nil {
		t.Error("parseTimeout(true) error = nil")
	}
}

func TestParseRequestRefusesUnknownKey(t *testing.T) {
	_, err := parseRequest(map[string]any{
		"url":   "http://localhost",
		"calls": []any{map[string]any{"method": "Core/echo"}},
		"cals":  []any{},
	})
	if err == nil || !strings.Contains(err.Error(), "not cals") {
		t.Errorf("parseRequest() error = %v, want one naming cals", err)
	}
}

// TestManifestsDeclare checks that the action.yml a release writes, and the
// one the e2e workflow uses, declare the params and the guard the action
// has, so that probe check and the guard of a run take it as it is.
func TestManifestsDeclare(t *testing.T) {
	checksums := filepath.Join(t.TempDir(), "checksums.txt")
	sum := strings.Repeat("a", 64)
	if err := os.WriteFile(checksums, []byte(sum+"  probe-jmap_linux_amd64\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	released, err := exec.Command("sh", "scripts/action-yml.sh", "v0.0.0", checksums).Output()
	if err != nil {
		t.Fatalf("scripts/action-yml.sh: %v", err)
	}
	local, err := os.ReadFile("e2e/jmap/action.yml")
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"scripts/action-yml.sh": released, "e2e/jmap/action.yml": local} {
		m, err := actionref.ParseManifest(data)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !slices.Equal(m.Params, params) {
			t.Errorf("%s declares params %v, want %v", name, m.Params, params)
		}
		if !slices.Equal(m.Guard, keeps) {
			t.Errorf("%s declares guard %v, want %v", name, m.Guard, keeps)
		}
	}
}
