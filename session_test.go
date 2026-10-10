package main

import (
	"net/http"
	"testing"

	"github.com/linyows/probe/actionrpc"
)

// job runs steps as a job does: each is given the state the one before it
// left, and a step that returns none leaves the state as it was.
type job struct {
	t     *testing.T
	state map[string]any
}

func (j *job) step(with map[string]any) map[string]any {
	j.t.Helper()
	ret, state, err := (&Action{}).RunStep(actionrpc.Call{With: with, State: j.state})
	if err != nil {
		j.t.Fatalf("RunStep() error = %v", err)
	}
	if state != nil {
		// The state goes to probe and back over gRPC.
		sendable, err := actionrpc.Sendable(state)
		if err != nil {
			j.t.Fatalf("state cannot be sent to probe: %v", err)
		}
		j.state = sendable
	}
	return ret
}

func sessionKept(ret map[string]any) any {
	return ret["req"].(map[string]any)["session_kept"]
}

var echoCall = []any{map[string]any{"method": "Core/echo"}}

func TestKeepSession(t *testing.T) {
	s := newJMAPServer(t, echo)
	s.state = "s1"
	j := &job{t: t}
	with := map[string]any{"url": s.URL, "keep_session": true, "calls": echoCall}

	first := j.step(with)
	second := j.step(with)

	if len(s.sessions) != 1 || len(s.posts) != 2 {
		t.Fatalf("got %d session and %d API requests, want 1 and 2", len(s.sessions), len(s.posts))
	}
	if sessionKept(first) != false || sessionKept(second) != true {
		t.Errorf("req.session_kept = %v then %v, want false then true", sessionKept(first), sessionKept(second))
	}
	for _, ret := range []map[string]any{first, second} {
		res := ret["res"].(map[string]any)
		if ret["status"] != 0 || res["session"].(map[string]any)["apiUrl"] != "/api/" {
			t.Errorf("status = %v, res.session = %v", ret["status"], res["session"])
		}
		if ret["req"].(map[string]any)["url"] != s.URL+"/api/" {
			t.Errorf("req.url = %v", ret["req"].(map[string]any)["url"])
		}
	}
}

// The session is kept for the URL and the credentials it was asked with, and
// only a step that keeps the session uses it.
func TestKeepSessionIsOfOneUser(t *testing.T) {
	s := newJMAPServer(t, echo)
	j := &job{t: t}
	alice := map[string]any{"url": s.URL, "keep_session": true, "calls": echoCall,
		"basic_auth": map[string]any{"username": "alice", "password": "a"}}
	bob := map[string]any{"url": s.URL, "keep_session": true, "calls": echoCall,
		"basic_auth": map[string]any{"username": "bob", "password": "b"}}

	j.step(alice)
	if ret := j.step(bob); sessionKept(ret) != false {
		t.Error("bob was given the session alice's step kept")
	}
	if ret := j.step(map[string]any{"url": s.URL, "calls": echoCall,
		"basic_auth": map[string]any{"username": "alice", "password": "a"}}); sessionKept(ret) != false {
		t.Error("a step that does not keep the session was given the one kept")
	}
	if ret := j.step(alice); sessionKept(ret) != true {
		t.Error("alice was not given the session her step kept")
	}
	if ret := j.step(bob); sessionKept(ret) != true {
		t.Error("bob was not given the session his step kept")
	}
	if len(s.sessions) != 3 {
		t.Errorf("got %d session requests, want 3", len(s.sessions))
	}
	if got := s.posts[3]["_header"].(http.Header).Get("Authorization"); got != "Basic YWxpY2U6YQ==" {
		t.Errorf("Authorization of alice's last request = %q", got)
	}
}

// The API names the session state with each response. When it is not the
// state of the session kept, the next step fetches the session again.
func TestKeepSessionFetchesAnOutdatedSessionAgain(t *testing.T) {
	s := newJMAPServer(t, echo)
	s.state = "s0"
	j := &job{t: t}
	with := map[string]any{"url": s.URL, "keep_session": true, "calls": echoCall}

	j.step(with)
	s.state = "s1"
	if ret := j.step(with); sessionKept(ret) != false {
		t.Error("the step was given a session the API had told was outdated")
	}
	if ret := j.step(with); sessionKept(ret) != true {
		t.Error("the step was not given the session that is up to date")
	}
	if len(s.sessions) != 2 {
		t.Errorf("got %d session requests, want 2", len(s.sessions))
	}
}

// A response that is not one the API gives a request it took, such as a 401
// for credentials that ran out, leaves no session kept.
func TestKeepSessionForgetsOnFailure(t *testing.T) {
	code := http.StatusOK
	s := newJMAPServer(t, func(w http.ResponseWriter, calls []any) {
		if code != http.StatusOK {
			w.WriteHeader(code)
			return
		}
		echo(w, calls)
	})
	j := &job{t: t}
	with := map[string]any{"url": s.URL, "keep_session": true, "calls": echoCall}

	j.step(with)
	code = http.StatusUnauthorized
	if ret := j.step(with); sessionKept(ret) != true || ret["status"] != 1 {
		t.Errorf("req.session_kept = %v, status = %v, want true and 1", sessionKept(ret), ret["status"])
	}
	code = http.StatusOK
	if ret := j.step(with); sessionKept(ret) != false || ret["status"] != 0 {
		t.Errorf("req.session_kept = %v, status = %v, want false and 0", sessionKept(ret), ret["status"])
	}
}

// A step without calls asks for the session itself, so it fetches it whatever
// is kept, and keeps what it got: a session, or none.
func TestKeepSessionAlone(t *testing.T) {
	s := newJMAPServer(t, echo)
	j := &job{t: t}
	alone := map[string]any{"url": s.URL, "keep_session": true}
	with := map[string]any{"url": s.URL, "keep_session": true, "calls": echoCall}

	j.step(alone)
	if ret := j.step(alone); sessionKept(ret) != false {
		t.Error("a step without calls was given the session kept")
	}
	if ret := j.step(with); sessionKept(ret) != true {
		t.Error("the step was not given the session a step without calls kept")
	}
	if len(s.sessions) != 2 {
		t.Fatalf("got %d session requests, want 2", len(s.sessions))
	}

	s.sessionCode = http.StatusUnauthorized
	if ret := j.step(alone); ret["status"] != 1 {
		t.Errorf("status = %v, want 1", ret["status"])
	}
	s.sessionCode = http.StatusOK
	if ret := j.step(with); sessionKept(ret) != false {
		t.Error("the step was given a session the server had since refused to give")
	}
}

func TestKeepSessionMustBeBoolean(t *testing.T) {
	_, err := parseRequest(map[string]any{"url": "http://x", "keep_session": "yes"})
	if err == nil {
		t.Error("parseRequest() error = nil for keep_session that is not true or false")
	}
}
