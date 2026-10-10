package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/linyows/probe/actionrpc"
)

// maxRedirects is how many redirects a request follows, as many as
// net/http follows when it is not told otherwise.
const maxRedirects = 10

// setErrors are the arguments of a /set response that hold the records it
// could not create, update or destroy, keyed by the record.
var setErrors = []string{"notCreated", "notUpdated", "notDestroyed"}

// response is what a request got back.
type response struct {
	code    int
	status  string
	headers map[string]any
	raw     []byte
	// url is where the response came from, after any redirect.
	url string
	// body is the body parsed as JSON, or nil when it is not JSON.
	body any
}

func (a *Action) do(r *request, guard actionrpc.Guard) (map[string]any, error) {
	ctx := context.Background()
	if r.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.timeout)
		defer cancel()
	}
	client := a.httpClient(guard)
	start := time.Now()

	sessionReq, err := a.newRequest(ctx, http.MethodGet, r.sessionURL, nil, r.headers)
	if err != nil {
		return nil, err
	}
	sessionRes, err := send(client, sessionReq, guard)
	if err != nil {
		return nil, err
	}
	session, apiURL, err := readSession(sessionRes)
	if err != nil {
		// The server answered, so what it answered is the result, with the
		// reason the methods were not sent.
		return result(r, nil, "", nil, sessionReq, sessionRes, []any{map[string]any{
			"kind":        "session",
			"description": err.Error(),
		}}, time.Since(start)), nil
	}

	methodCalls := make([]any, 0, len(r.calls))
	for _, c := range r.calls {
		args := maps.Clone(c.args)
		if id := r.accountFor(c.method, session); id != "" && !hasArg(args, "accountId") {
			args["accountId"] = id
		}
		methodCalls = append(methodCalls, []any{c.method, args, c.id})
	}
	body, err := json.Marshal(map[string]any{"using": r.using, "methodCalls": methodCalls})
	if err != nil {
		return nil, fmt.Errorf("with.calls cannot be sent as JSON: %w", err)
	}
	apiReq, err := a.newRequest(ctx, http.MethodPost, apiURL, body, r.headers)
	if err != nil {
		return nil, err
	}
	apiReq.Header.Set("Content-Type", "application/json")
	apiRes, err := send(client, apiReq, guard)
	if err != nil {
		return nil, err
	}
	return result(r, session, apiURL, methodCalls, apiReq, apiRes, nil, time.Since(start)), nil
}

// httpClient returns the client the requests are sent with, which follows
// a redirect only to a host the guard allows.
func (a *Action) httpClient(guard actionrpc.Guard) *http.Client {
	client := &http.Client{}
	if a.client != nil {
		c := *a.client
		client = &c
	}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return fmt.Errorf("stopped after %d redirects", maxRedirects)
		}
		return checkHost(guard, req.URL)
	}
	return client
}

func (a *Action) newRequest(ctx context.Context, method, target string, body []byte, headers map[string]string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "probe-jmap/"+version)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req, nil
}

// send sends req to a host the guard allows, and reads the response.
func send(client *http.Client, req *http.Request, guard actionrpc.Guard) (*response, error) {
	if err := checkHost(guard, req.URL); err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		// A redirect the guard refused is a refusal, not a failure to reach
		// the server.
		if refused := (*actionrpc.Refused)(nil); errors.As(err, &refused) {
			return nil, refused
		}
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	res := &response{code: resp.StatusCode, status: resp.Status, headers: flattenHeaders(resp.Header), raw: raw, url: resp.Request.URL.String()}
	var parsed any
	if json.Unmarshal(raw, &parsed) == nil {
		res.body = parsed
	}
	return res, nil
}

// checkHost returns a Refused error when the guard does not allow the host
// of u. A host without a port is taken at the port of its scheme.
func checkHost(guard actionrpc.Guard, u *url.URL) error {
	host := u.Host
	if u.Port() == "" {
		port := "80"
		if u.Scheme == "https" {
			port = "443"
		}
		host = net.JoinHostPort(u.Hostname(), port)
	}
	return guard.CheckHost(host)
}

// readSession returns the session in res and its API URL, resolved against
// the URL the session came from after any redirect, or why res holds no
// session.
func readSession(res *response) (map[string]any, string, error) {
	if res.code < 200 || res.code > 299 {
		return nil, "", fmt.Errorf("the session request got %s", res.status)
	}
	session, ok := res.body.(map[string]any)
	if !ok {
		return nil, "", errors.New("the session is not a JSON object")
	}
	api, _ := session["apiUrl"].(string)
	if api == "" {
		return nil, "", errors.New("the session has no apiUrl")
	}
	base, err := url.Parse(res.url)
	if err != nil {
		return nil, "", err
	}
	ref, err := url.Parse(api)
	if err != nil {
		return nil, "", fmt.Errorf("the apiUrl of the session is not a URL: %w", err)
	}
	return session, base.ResolveReference(ref).String(), nil
}

// accountFor returns the account a call of method is made in when its args
// name none: with.account_id, or else the primary account of the
// capability that defines method. A method of an accountless type is made
// in no account.
func (r *request) accountFor(method string, session map[string]any) string {
	if typ, _, _ := strings.Cut(method, "/"); slices.Contains(accountless, typ) {
		return ""
	}
	if r.accountID != "" {
		return r.accountID
	}
	capability, ok := capabilityOf(method)
	if !ok {
		return ""
	}
	primary, _ := session["primaryAccounts"].(map[string]any)
	id, _ := primary[capability].(string)
	return id
}

// hasArg reports whether args has name, as a value or as a back-reference.
func hasArg(args map[string]any, name string) bool {
	_, value := args[name]
	_, ref := args["#"+name]
	return value || ref
}

// result is what the step gets: the request last sent and its response.
// errs holds the errors found before the method responses, if any.
func result(r *request, session map[string]any, apiURL string, methodCalls []any, req *http.Request, res *response, errs []any, rt time.Duration) map[string]any {
	responses := []any{}
	results := map[string]any{}
	if errs == nil {
		errs = []any{}
	}

	methods := map[string]string{}
	for _, c := range r.calls {
		methods[c.id] = c.method
	}
	ok := res.code >= 200 && res.code <= 299
	obj, isObj := res.body.(map[string]any)
	if !isObj {
		ok = false
	}
	if session != nil {
		if !ok && isObj {
			// A request the server refused as a whole is answered with a
			// problem details object, as RFC 8620 section 3.6.1 sets.
			if _, has := obj["type"]; has {
				errs = append(errs, withFields(obj, map[string]any{"kind": "request"}))
			}
		}
		list, isList := obj["methodResponses"].([]any)
		if ok && !isList {
			ok = false
		}
		for _, item := range list {
			inv, isInv := item.([]any)
			if !isInv || len(inv) != 3 {
				continue
			}
			name, _ := inv[0].(string)
			args, _ := inv[1].(map[string]any)
			id, _ := inv[2].(string)
			responses = append(responses, map[string]any{"name": name, "args": args, "id": id})
			// A call may get more than one response, such as the implicit
			// Email/set of an Email/copy; results keeps the first.
			if _, seen := results[id]; !seen {
				results[id] = args
			}
			if name == "error" {
				errs = append(errs, withFields(args, map[string]any{"kind": "method", "id": id, "method": methods[id]}))
				continue
			}
			for _, kind := range setErrors {
				failed, _ := args[kind].(map[string]any)
				for key, e := range failed {
					setErr, _ := e.(map[string]any)
					errs = append(errs, withFields(setErr, map[string]any{"kind": kind, "id": id, "method": name, "key": key}))
				}
			}
		}
	}

	status := 0
	if !ok || len(errs) > 0 {
		status = 1
	}

	resMap := map[string]any{
		"code":      res.code,
		"status":    res.status,
		"headers":   res.headers,
		"body":      string(res.raw),
		"session":   nil,
		"responses": responses,
		"results":   results,
		"errors":    errs,
	}
	if session != nil {
		resMap["session"] = session
	}
	if res.body != nil {
		resMap["body"] = res.body
		resMap["rawbody"] = string(res.raw)
	}

	return map[string]any{
		"req": map[string]any{
			"session_url": r.sessionURL,
			"url":         apiURL,
			"using":       r.using,
			"calls":       methodCalls,
			"headers":     flattenHeaders(req.Header),
		},
		"res":    resMap,
		"rt":     rt.String(),
		"status": status,
	}
}

// withFields returns a copy of m with fields added.
func withFields(m map[string]any, fields map[string]any) map[string]any {
	out := maps.Clone(m)
	if out == nil {
		out = map[string]any{}
	}
	maps.Copy(out, fields)
	return out
}

func flattenHeaders(h http.Header) map[string]any {
	m := make(map[string]any, len(h))
	for k, v := range h {
		m[k] = strings.Join(v, ", ")
	}
	return m
}
