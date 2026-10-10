package main

import (
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"slices"
	"strings"
)

// stateSessionsKey names the sessions in the state the action keeps in a job.
const stateSessionsKey = "sessions"

// sessionKey names the session r asks for among those a job keeps: the
// session URL and the headers it is asked with, so that the session of one
// user is not used for another. It is a digest, as the headers hold the
// credentials. The case of a header name does not tell sessions apart.
func (r *request) sessionKey() string {
	// The names are put in one case before they are put in order, as the
	// same headers written in another case ask for the same session.
	lines := make([]string, 0, len(r.headers))
	for k, v := range r.headers {
		lines = append(lines, strings.ToLower(k)+": "+v)
	}
	slices.Sort(lines)
	h := sha256.New()
	h.Write([]byte(r.sessionURL))
	for _, line := range lines {
		h.Write([]byte("\n" + line))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// keptSession returns the session state keeps under key and the API URL it
// was resolved to.
func keptSession(state map[string]any, key string) (session map[string]any, apiURL string, ok bool) {
	sessions, _ := state[stateSessionsKey].(map[string]any)
	kept, _ := sessions[key].(map[string]any)
	session, _ = kept["session"].(map[string]any)
	apiURL, _ = kept["api_url"].(string)
	return session, apiURL, session != nil && apiURL != ""
}

// withSession returns state with session kept under key, or without one
// there when session is nil. state itself is left as it is.
func withSession(state map[string]any, key string, session map[string]any, apiURL string) map[string]any {
	sessions := map[string]any{}
	if kept, ok := state[stateSessionsKey].(map[string]any); ok {
		sessions = maps.Clone(kept)
	}
	if session == nil {
		delete(sessions, key)
	} else {
		sessions[key] = map[string]any{"session": session, "api_url": apiURL}
	}
	out := maps.Clone(state)
	if out == nil {
		out = map[string]any{}
	}
	out[stateSessionsKey] = sessions
	return out
}

// outdated reports whether res, the response of the API, tells that session
// is no longer the one the server gives: it names another session state, as
// RFC 8620 section 3.4 has it do, or it is not a response the API gives a
// request it took.
func outdated(session map[string]any, res *response) bool {
	if res.code < 200 || res.code > 299 {
		return true
	}
	body, _ := res.body.(map[string]any)
	now, ok := body["sessionState"].(string)
	if !ok {
		return false
	}
	was, ok := session["state"].(string)
	return ok && was != now
}
