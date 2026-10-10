package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/linyows/probe/actionrpc"
)

// DefaultTimeout bounds a step that does not set with.timeout.
const DefaultTimeout = 30 * time.Second

// coreCapability is the capability every JMAP server has.
const coreCapability = "urn:ietf:params:jmap:core"

// capabilities maps the data type a method name starts with to the
// capability that defines it, for the specifications published as RFCs.
var capabilities = map[string]string{
	"Core":             coreCapability,
	"PushSubscription": coreCapability,
	"Mailbox":          "urn:ietf:params:jmap:mail",
	"Thread":           "urn:ietf:params:jmap:mail",
	"Email":            "urn:ietf:params:jmap:mail",
	"SearchSnippet":    "urn:ietf:params:jmap:mail",
	"Identity":         "urn:ietf:params:jmap:submission",
	"EmailSubmission":  "urn:ietf:params:jmap:submission",
	"VacationResponse": "urn:ietf:params:jmap:vacationresponse",
	"MDN":              "urn:ietf:params:jmap:mdn",
	"Blob":             "urn:ietf:params:jmap:blob",
	"Quota":            "urn:ietf:params:jmap:quota",
	"SieveScript":      "urn:ietf:params:jmap:sieve",
	"AddressBook":      "urn:ietf:params:jmap:contacts",
	"ContactCard":      "urn:ietf:params:jmap:contacts",
}

// methodCapabilities maps a method to the capability that defines it, where
// that is not the one that defines the other methods of its type: Blob/copy
// is a method of RFC 8620, and the rest of Blob of RFC 9404.
var methodCapabilities = map[string]string{
	"Blob/copy": coreCapability,
}

// accountless are the types whose methods take no accountId: Core/echo, and
// a push subscription, which RFC 8620 section 7.2 ties to no account.
var accountless = []string{"Core", "PushSubscription"}

// params are the keys the action takes in with. action.yml declares them,
// for probe check to report a key the action does not take.
var params = []string{"url", "calls", "using", "using_also", "account_id", "basic_auth", "headers", "timeout"}

// keeps are the kinds of guard the action keeps to, as action.yml declares
// them: it refuses a method that may write, and a host the run does not
// allow.
var keeps = []string{actionrpc.KindReadOnly, actionrpc.KindAllowHost}

// readMethods are the methods, after the data type, that only read. They
// are what a read-only run sends.
var readMethods = []string{"get", "query", "changes", "queryChanges", "lookup", "echo"}

// Action calls the JMAP methods a step describes and returns the responses.
type Action struct {
	log hclog.Logger
	// client sends the requests. It defaults to http.DefaultTransport.
	client *http.Client
}

// Run calls the methods in with, under no guard.
func (a *Action) Run(with map[string]any) (map[string]any, error) {
	ret, _, err := a.RunStep(actionrpc.Call{With: with})
	return ret, err
}

// RunStep calls the methods in call.With: it fetches the session, then
// sends the method calls to the API URL the session names. A step without
// calls stops at the session. A response the
// server sends is a result, whatever its status code or method errors; only
// a request that gets no response, or one the guard refuses, is an error.
func (a *Action) RunStep(call actionrpc.Call) (map[string]any, map[string]any, error) {
	if a.log == nil {
		a.log = hclog.NewNullLogger()
	}
	actionrpc.LogParams(a.log, "received request parameters", call.With)

	req, err := parseRequest(call.With)
	if err != nil {
		return nil, nil, err
	}
	if err := checkReadOnly(call.Guard, req.calls); err != nil {
		return nil, nil, err
	}
	ret, err := a.do(req, call.Guard)
	actionrpc.LogOutcome(a.log, "jmap request", ret, err)
	return ret, nil, err
}

type request struct {
	// sessionURL is where the session is fetched from.
	sessionURL string
	using      []string
	calls      []methodCall
	accountID  string
	headers    map[string]string
	timeout    time.Duration
}

type methodCall struct {
	method string
	args   map[string]any
	id     string
}

func parseRequest(with map[string]any) (*request, error) {
	r := &request{timeout: DefaultTimeout, headers: map[string]string{}}

	for k := range with {
		if !slices.Contains(params, k) {
			return nil, fmt.Errorf("jmap action takes %s, not %s", strings.Join(params, ", "), k)
		}
	}

	raw, _ := with["url"].(string)
	if raw == "" {
		return nil, errors.New("jmap action requires with.url")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("with.url must be an http or https URL, not %q", raw)
	}
	// A URL without a path is the server, whose session is at the
	// well-known URI that RFC 8620 section 2.2 sets.
	if u.Path == "" || u.Path == "/" {
		u.Path = "/.well-known/jmap"
	}
	r.sessionURL = u.String()

	// A step without with.calls fetches the session alone. What only the
	// calls use is left unread, as the defaults of a job may give it to a
	// step that makes none.
	calls, hasCalls := with["calls"]
	if !hasCalls {
		if err := r.parseTransport(with); err != nil {
			return nil, err
		}
		return r, nil
	}
	if r.calls, err = parseCalls(calls); err != nil {
		return nil, err
	}

	// with.using is the whole of what is sent; with.using_also is added to
	// what the calls are inferred to use.
	var also []string
	if v, ok := with["using_also"]; ok && v != nil {
		if also, err = stringList(v, "with.using_also"); err != nil {
			return nil, err
		}
	}
	if v, ok := with["using"]; ok && v != nil {
		if also != nil {
			return nil, errors.New("with.using and with.using_also cannot be given together: using is sent as written, and using_also is added to what is inferred")
		}
		if r.using, err = stringList(v, "with.using"); err != nil {
			return nil, err
		}
	} else if r.using, err = inferUsing(r.calls, also); err != nil {
		return nil, err
	}

	if v, ok := with["account_id"]; ok {
		if r.accountID, ok = v.(string); !ok {
			return nil, fmt.Errorf("with.account_id must be a string, not %T", v)
		}
	}

	if err := r.parseTransport(with); err != nil {
		return nil, err
	}
	return r, nil
}

// parseTransport reads what the session request and the API request share:
// with.headers, with.basic_auth and with.timeout.
func (r *request) parseTransport(with map[string]any) error {
	var err error
	if v, ok := with["headers"]; ok && v != nil {
		headers, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("with.headers must be an object, not %T", v)
		}
		for k, v := range headers {
			r.headers[k] = fmt.Sprint(v)
		}
	}

	if v, ok := with["basic_auth"]; ok {
		auth, err := basicAuth(v)
		if err != nil {
			return err
		}
		for k := range r.headers {
			if strings.EqualFold(k, "Authorization") {
				return errors.New("with.basic_auth and an Authorization header cannot be given together")
			}
		}
		r.headers["Authorization"] = auth
	}

	if v, ok := with["timeout"]; ok {
		if r.timeout, err = parseTimeout(v); err != nil {
			return err
		}
	}

	return nil
}

// parseCalls reads with.calls, a list of method calls that is not empty. A
// step leaves the key out to make none; a list that is empty or null is
// more likely one that lost its calls. A call without an id
// is given "c" and its position, from 0. A back-reference without a name is
// given the method of the call it refers to.
func parseCalls(v any) ([]methodCall, error) {
	list, ok := v.([]any)
	if !ok || len(list) == 0 {
		return nil, errors.New("with.calls must be a list of method calls that is not empty; a step that fetches the session alone leaves calls out")
	}
	calls := make([]methodCall, 0, len(list))
	methods := map[string]string{}
	for i, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("with.calls[%d] must be an object of method, args and id, not %T", i, item)
		}
		for k := range m {
			if k != "method" && k != "args" && k != "id" {
				return nil, fmt.Errorf("with.calls[%d] takes method, args and id, not %s", i, k)
			}
		}
		c := methodCall{id: fmt.Sprintf("c%d", i), args: map[string]any{}}
		if c.method, _ = m["method"].(string); c.method == "" {
			return nil, fmt.Errorf("with.calls[%d].method is required, such as Email/get", i)
		}
		if !strings.Contains(c.method, "/") {
			return nil, fmt.Errorf("with.calls[%d].method must be a type and a method, such as Email/get, not %q", i, c.method)
		}
		if v, ok := m["id"]; ok {
			if c.id, ok = v.(string); !ok || c.id == "" {
				return nil, fmt.Errorf("with.calls[%d].id must be a string that is not empty", i)
			}
		}
		if _, dup := methods[c.id]; dup {
			return nil, fmt.Errorf("with.calls[%d].id %q is the id of an earlier call", i, c.id)
		}
		if v, ok := m["args"]; ok && v != nil {
			args, ok := v.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("with.calls[%d].args must be an object, not %T", i, v)
			}
			c.args = args
		}
		nameBackReferences(c.args, methods)
		methods[c.id] = c.method
		calls = append(calls, c)
	}
	return calls, nil
}

// nameBackReferences gives each back-reference in args, an argument whose
// name starts with "#", the name of the method it refers to when it has
// none, as RFC 8620 section 3.7 requires one.
func nameBackReferences(args map[string]any, methods map[string]string) {
	for k, v := range args {
		if !strings.HasPrefix(k, "#") {
			continue
		}
		ref, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if _, ok := ref["name"]; ok {
			continue
		}
		of, _ := ref["resultOf"].(string)
		if method, ok := methods[of]; ok {
			ref["name"] = method
		}
	}
}

// inferUsing returns the capabilities the methods of calls belong to, after
// the core capability, and then those of also that are not among them. A
// method of a type that is not known is taken to belong to one of also, and
// needs with.using when there is none.
func inferUsing(calls []methodCall, also []string) ([]string, error) {
	using := []string{coreCapability}
	for _, c := range calls {
		capability, ok := capabilityOf(c.method)
		if !ok {
			if len(also) > 0 {
				continue
			}
			return nil, fmt.Errorf("with.using or with.using_also is needed for %s, as probe-jmap does not know which capability defines it", c.method)
		}
		if !slices.Contains(using, capability) {
			using = append(using, capability)
		}
	}
	for _, capability := range also {
		if !slices.Contains(using, capability) {
			using = append(using, capability)
		}
	}
	return using, nil
}

// capabilityOf returns the capability that defines method.
func capabilityOf(method string) (string, bool) {
	if capability, ok := methodCapabilities[method]; ok {
		return capability, true
	}
	typ, _, _ := strings.Cut(method, "/")
	capability, ok := capabilities[typ]
	return capability, ok
}

// checkReadOnly returns a Refused error when the run is read-only and one of
// calls may write: a method that is not one of readMethods.
func checkReadOnly(guard actionrpc.Guard, calls []methodCall) error {
	if !guard.ReadOnly {
		return nil
	}
	for _, c := range calls {
		_, name, _ := strings.Cut(c.method, "/")
		if !slices.Contains(readMethods, name) {
			return actionrpc.Refuse("%s may write, and the run is read-only; only the methods %s are sent", c.method, strings.Join(readMethods, ", "))
		}
	}
	return nil
}

func stringList(v any, field string) ([]string, error) {
	list, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("%s must be a list of strings, not %T", field, v)
	}
	out := make([]string, 0, len(list))
	for i, item := range list {
		s, ok := item.(string)
		if !ok || s == "" {
			return nil, fmt.Errorf("%s[%d] must be a string that is not empty", field, i)
		}
		out = append(out, s)
	}
	return out, nil
}

// basicAuth builds the value of an Authorization header for HTTP Basic
// authentication from with.basic_auth, a map of username and password, as
// the http action takes it.
func basicAuth(v any) (string, error) {
	auth, ok := v.(map[string]any)
	if !ok {
		return "", errors.New("with.basic_auth must be a map of username and password")
	}
	for k := range auth {
		if k != "username" && k != "password" {
			return "", fmt.Errorf("with.basic_auth takes username and password, not %s", k)
		}
	}
	username, _ := auth["username"].(string)
	if username == "" {
		return "", errors.New("with.basic_auth.username is required")
	}
	if strings.Contains(username, ":") {
		return "", errors.New("with.basic_auth.username must not contain a colon")
	}
	password, ok := auth["password"].(string)
	if !ok && auth["password"] != nil {
		return "", errors.New("with.basic_auth.password must be a string")
	}
	req, _ := http.NewRequest(http.MethodGet, "http://localhost", nil)
	req.SetBasicAuth(username, password)
	return req.Header.Get("Authorization"), nil
}

// parseTimeout accepts a duration string such as "10s" or a number of
// seconds, as the http action does.
func parseTimeout(v any) (time.Duration, error) {
	switch t := v.(type) {
	case string:
		d, err := time.ParseDuration(t)
		if err != nil {
			return 0, fmt.Errorf("with.timeout: %w", err)
		}
		return d, nil
	case int:
		return time.Duration(t) * time.Second, nil
	case int64:
		return time.Duration(t) * time.Second, nil
	case float64:
		return time.Duration(t * float64(time.Second)), nil
	default:
		return 0, fmt.Errorf("with.timeout must be a duration or a number of seconds, not %T", v)
	}
}
