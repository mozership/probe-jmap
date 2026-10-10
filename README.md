# probe-jmap

A [Probe](https://github.com/mozership/probe) action that calls [JMAP](https://jmap.io/) methods ([RFC 8620](https://www.rfc-editor.org/rfc/rfc8620), [RFC 8621](https://www.rfc-editor.org/rfc/rfc8621)).

A step names the server and the method calls. The action fetches the session, fills in the account of each call, sends the calls in one request, and returns the responses by call id, with the method errors gathered in one list. A step that names no calls fetches the session alone.

Probe downloads it the first time a workflow uses it. It needs Probe v1.21.0 or later, which reads the guard and the params it declares in `action.yml`.

```yaml
name: Mail over JMAP
vars:
  user: "{{JMAP_USER}}"
  pass: "{{JMAP_PASS}}"
jobs:
- name: jmap
  defaults:
    github.com/mozership/probe-jmap@<commit SHA>:
      url: https://jmap.example.com
      basic_auth:
        username: "{{vars.user}}"
        password: "{{vars.pass}}"
  steps:
  - name: Read the latest message
    uses: github.com/mozership/probe-jmap@<commit SHA>
    with:
      calls:
      - method: Email/query
        id: latest
        args:
          sort: [{property: receivedAt, isAscending: false}]
          limit: 1
      - method: Email/get
        id: email
        args:
          "#ids": {resultOf: latest, path: /ids}
          properties: [subject, from, receivedAt]
    test: status == 0 && len(res.results.email.list) == 1
    echo: "Latest: {{res.results.email.list[0].subject}}"
```

Probe only takes a full 40-character commit SHA. The notes of each [release](https://github.com/mozership/probe-jmap/releases) start with the `uses` line to copy.

## Parameters

| Parameter | Type | Required | Default | Description |
|---|---|---|---|---|
| `url` | String | Yes | - | The server, `http` or `https`. A URL with no path is taken as the server, whose session is at `/.well-known/jmap`; a URL with a path is the session URL itself |
| `calls` | Array | No | - | The method calls, sent in one request in this order. A step without the key [fetches the session alone](#the-session-alone); a list that is empty is an error |
| `using` | Array | No | inferred | The capabilities the request uses, sent as written. By default the core capability and those that define the methods in `calls` |
| `using_also` | Array | No | - | Capabilities to add to the inferred ones, such as the one of a vendor. Not a key of JMAP, and not given together with `using` |
| `account_id` | String | No | primary account | The account of each call whose `args` give no `accountId` |
| `keep_session` | Boolean | No | `false` | Keeps the session in the job, so that the later steps that keep it send their calls without fetching it again. See [Keeping the session](#keeping-the-session) |
| `basic_auth` | Object | No | - | `username` and `password` for HTTP Basic authentication |
| `headers` | Object | No | - | Request headers, such as `Authorization: Bearer <token>` |
| `timeout` | Duration | No | `30s` | Time limit for both requests together, as `10s` or a number of seconds. `0` removes it |

A call in `calls` takes:

| Field | Required | Description |
|---|---|---|
| `method` | Yes | The method name, such as `Email/get` |
| `args` | No | The arguments of the method |
| `id` | No | The call id, which a back-reference and `res.results` use. Defaults to `c` and the position of the call, from `c0` |

A call whose `args` give neither `accountId` nor `#accountId` is made in `account_id`, or else in the primary account the session names for the capability of the method. `Core/echo` and the methods of `PushSubscription` take no `accountId`, and are given none.

A back-reference, an argument whose name starts with `#`, may leave out `name`: it is given the method of the call that `resultOf` names.

### Capabilities

`using` is inferred from the type in the method name:

| Type | Capability |
|---|---|
| `Core`, `PushSubscription` | `urn:ietf:params:jmap:core` |
| `Mailbox`, `Thread`, `Email`, `SearchSnippet` | `urn:ietf:params:jmap:mail` |
| `Identity`, `EmailSubmission` | `urn:ietf:params:jmap:submission` |
| `VacationResponse` | `urn:ietf:params:jmap:vacationresponse` |
| `MDN` | `urn:ietf:params:jmap:mdn` |
| `Blob` | `urn:ietf:params:jmap:blob` |
| `Quota` | `urn:ietf:params:jmap:quota` |
| `SieveScript` | `urn:ietf:params:jmap:sieve` |
| `AddressBook`, `ContactCard` | `urn:ietf:params:jmap:contacts` |

`Blob/copy` is the exception: RFC 8620 defines it, so it is inferred as the core capability, and it is made in the primary account of that capability when the session names one.

A method of any other type needs `using` or `using_also`:

- `using` replaces what is inferred, and is sent as written. It is how to leave a capability out, or to send one the server does not have.
- `using_also` is added after what is inferred, without a capability twice. A method of a type that is not in the table is taken to belong to one of them, so the standard types and those of a vendor are written together without listing every capability:

```yaml
with:
  using_also: [urn:example:vendor]
  calls:
  - method: Mailbox/get
  - method: x:Domain/query
```

This sends `using` as the core capability, `urn:ietf:params:jmap:mail` and `urn:example:vendor`. The method of the vendor is given no `accountId`, as its capability is not known.

A step cannot give both. When the `defaults` of a job give `using_also`, a step that gives `using` sets `using_also: null`.

A key of `with` that is not one of the parameters above fails the step before anything is sent, and `probe check` reports it with its line.

### The session alone

A step without `calls` fetches the session and sends nothing to the API. It is how to wait for a server, or to test what the session says:

```yaml
- name: Wait for the server
  uses: github.com/mozership/probe-jmap@<commit SHA>
  retry: {max_attempts: 60, interval: 1s}
  test: status == 0 && res.session.capabilities["urn:ietf:params:jmap:mail"] != nil
```

`res` is then the response to the session request, `res.results` and `res.responses` are empty, and `status` is `0` when the server gave a session. `using`, `using_also` and `account_id` are for the calls, and such a step does not read them, so the `defaults` of a job may give them.

Only the key left out does this. `calls` with an empty list or no value fails the step, as a list that lost its calls would otherwise pass for a step that asks for none.

### Keeping the session

The action fetches the session in every step. With `keep_session: true`, a step keeps the session it fetched in the job, and a later step of the job that keeps the session too sends its calls with that one, in one request instead of two. In the `defaults` of a job it applies to every step:

```yaml
jobs:
- name: jmap
  defaults:
    github.com/mozership/probe-jmap@<commit SHA>:
      url: https://jmap.example.com
      keep_session: true
```

Nothing is fetched before the first step: the first step that keeps the session fetches it, as it would without `keep_session`.

- The session is kept for the session URL and the headers it was asked with, `basic_auth` among them, so a step of another user fetches its own.
- A step without `calls` asks for the session itself: it always fetches it, and keeps what it got.
- The API names the state of the session in each response. When that is not the `state` of the session kept, or the API answers with a status other than 2xx, the session is dropped, and the next step fetches it again.
- A session the server did not give is not kept.
- The session is kept for one run of the job, and is not shared with another job.

`req.session_kept` tells whether `res.session` is the session the job kept or one fetched for the step, and `rt` is then the time of the API request alone. With a session kept, credentials the server no longer takes show as the response of the API, not as an error of the `session` kind.

## Result

| Field | Type | Description |
|---|---|---|
| `res.code` | Integer | HTTP status code |
| `res.status` | String | HTTP status line, such as `"200 OK"` |
| `res.headers` | Object | Response headers, keyed by canonical name |
| `res.session` | Object | The session object as the server sent it, to this step or to the one that [kept it](#keeping-the-session), or `null` when there is none |
| `res.results` | Object | The arguments of each method response, keyed by call id. A method error is there too, as the error object |
| `res.responses` | Array | Every method response in order, as `name`, `args` and `id` |
| `res.errors` | Array | What failed, described below; empty when nothing did |
| `res.body` | Any | The whole response body, parsed when it is JSON, otherwise the raw string |
| `res.rawbody` | String | The unparsed body, present when the body is JSON |
| `req` | Object | The `session_url`, `session_kept`, the API `url`, `using`, the `calls` as sent and the `headers`. In a step without `calls`, `url` is `""` and `using` and `calls` are empty |
| `rt` | Duration | Time for the session and the API request together, or for the API request alone when the session was kept |
| `status` | Integer | `0` when the API answered 2xx with method responses and `res.errors` is empty, or, in a step without `calls`, when the server gave a session; `1` otherwise |

A JMAP server answers a method it could not run with HTTP 200, so `res.code` alone does not tell that the calls succeeded. `res.errors` gathers every failure, each the error object the server sent with these fields added:

| `kind` | What failed | Added fields |
|---|---|---|
| `method` | A method call, answered with an `error` response | `id`, `method` |
| `notCreated`, `notUpdated`, `notDestroyed` | A record of a `/set` call | `id`, `method`, `key` (the creation id or the record id) |
| `request` | The whole request, answered with a problem details object | - |
| `session` | The session, which the server did not give | `description` |

When the server gives no session, the method calls are not sent, and `res` is the response to the session request, as it is in a step without `calls`.

Any response the server sends is a result, so a test can assert on a method error or a 401. Only a request that gets no response, such as a refused connection or a timeout, fails the step as an error.

## Guard

The action keeps to the guard of the run, and its `action.yml` declares `guard: [read-only, allow-host]`, so Probe runs it under `--read-only` and `--allow-host` without `--allow-action`.

Under `--read-only`, a step whose calls include a method other than `/get`, `/query`, `/changes`, `/queryChanges`, `/lookup` and `/echo` is refused before anything is sent. A host that `--allow-host` does not allow is refused, for the session, the API URL and any redirect.

## Testing

`go test ./...` runs against stub servers. `e2e/workflow.yml` starts Stalwart in Docker, sets it up, and runs the action through Probe against it:

```sh
go build -o e2e/jmap/probe-jmap . && probe e2e/workflow.yml
```

## Releasing

Pushing a `v*` tag builds the executables with GoReleaser and publishes them on the release. The workflow then commits an `action.yml` with their URLs and SHA-256 digests to `main`, and adds the commit to the release notes. That commit is the one to pin: Probe reads `action.yml` at the pinned commit and refuses an executable whose digest differs.

## License

MIT
