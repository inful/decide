# decide

A small, dependency-free Go client for the `/v1/systemone` decision API.

The API accepts unstructured context (`state.body`) and a map of structured
questions (`questions.<key>`), and answers each question according to its type
and instructions. See the [example exchange](#example-exchange) below.

## Install

```
go get github.com/inful/decide
```

This module requires Go 1.26 or newer, and `go.mod` says so. A consumer on
an older Go picks up the newer toolchain by itself; only a pinned
`GOTOOLCHAIN=local` stops that. CI tests on Go 1.26, the version the module
requires, and on the current stable release.

## Quick start

```go
client := decide.New(
    "https://api.example.com",
    decide.WithToken("<TOKEN>"),
)

resp, err := client.SystemOne(ctx, decide.Request{
    State: decide.State{Body: "I was charged twice, I want a refund."},
    Questions: decide.Questions{
        "department": decide.NewChoice(
            "Which department should handle this?",
            map[string]string{
                "billing":   "invoices, payments, refunds",
                "technical": "bugs and outages",
                "other":     "everything else",
            },
        ),
        "refund_requested": decide.NewNoul("Does the user request a refund?"),
    },
})
if err != nil {
    return err
}

// resp.Answers is map[string]decide.Answer.
if dept := resp.Answers["department"]; dept.IsChoice() && dept.ChoiceKey() == "billing" {
    routeTo(refundQueue, resp.Answers["refund_requested"].NoulValue())
}
```

## Question types

Three types are modeled today:

| Type    | Constructor                       | Response fields                    |
|---------|-----------------------------------|------------------------------------|
| choice  | `NewChoice(instr, criteria)`      | `Choice`, `Probabilities`, `Confidence` |
| score   | `NewScore(instr, levels)`         | `Score`, `Legend`, `Probabilities`, `Confidence` |
| noul    | `NewNoul(instr)`                  | `Noul` (a `[0,1]` prob)            |

A `choice` question's criteria are an object mapping option keys to
descriptions. A `score` question's are an ordered list of level descriptions,
lowest first, and the answer is the expected level — it can land between two
levels. The two shapes are sent differently, so `Question` marshals them
itself; `NewScore` takes a `[]string` and sends it as an array.

The client sends the instructions as text and the choice descriptions as
strings. The API also accepts a structured instruction (an object or an
array) and a `null` description; this client sends neither.

### What the confidence numbers mean

They are not the same thing, and neither is "the chance the answer is right":

- `Confidence` — how *concentrated* the answer's probabilities are, from 0 to 1.
  It is `1 − H(p)/log k` over the option distribution, so a spread-out answer
  scores low even when the top option leads.
- `AnswerConfidence` — the top option's own probability.
- `Action.ActProbability` — the model's act/escalate head. Upstream reports
  it carries little signal, so **gate on `Confidence`**, not on this.

Not every server sends every field. Ollama sends `Confidence` for choice and
score answers but **no** `confidence` for a noul answer, and it sends neither
`answer_confidence` nor `action` — those come back as zero. Use `NoulValue()`
for a noul answer and treat a zero `Confidence` on a noul as "not reported",
not as "no confidence".

### Truncation is the server's call

The server decides how much of your state it reads, and it may cut the rest
without saying so. The English Laya checkpoint reads 512 tokens per question,
so a long body is cut at that point. `Usage.Truncated` and `Usage.StateTokens`
are only filled in by servers that report them — Ollama reports neither — so
they come back false and zero even when the input *was* cut. Watch
`Usage.InputTokens` against the server's budget if it matters.

### The request has a size limit too

The server caps the request body and answers a bigger one with **413**. Ollama
caps it at 64 KiB; the hosted API at 32 MiB. The client sends what you give it
and does not check the size itself, so an oversized request comes back as an
error — `ErrBadRequest`, because a smaller request is the fix.

The context window is a separate limit, and the way to raise it is a header
rather than a body field. The hosted API reads 64k tokens per request by
default and 262k with the long-context header set, so a state that does not
fit needs it:

```go
client := decide.New(baseURL,
    decide.WithToken(token),
    decide.WithLongContext(),
)
```

The larger window is billed per token, so it costs more per request. Servers
that do not know the header — Ollama, Laya's own server — ignore it.

## Errors

`Client.SystemOne` returns either a `*decide.Response` or a `*decide.Error`. Use
`errors.Is` to branch on category:

```go
_, err := client.SystemOne(ctx, req)
switch {
case errors.Is(err, decide.ErrBadRequest):     // 4xx — fix the request
case errors.Is(err, decide.ErrUnauthorized):   // 401 — check the token
case errors.Is(err, decide.ErrRateLimited):    // 429 — back off (already retried)
case errors.Is(err, decide.ErrServerError):    // 5xx — back off (already retried)
case errors.Is(err, decide.ErrTransport):      // network/dial/TLS (already retried)
case errors.Is(err, decide.ErrResponseDecoded):// unexpected payload shape
case errors.Is(err, decide.ErrResponseTooLarge):// body past the 8 MiB limit
case err != nil:
    return err
}
```

Every 4xx that is not 401, 403, 404 or 429 is `ErrBadRequest` — 413 for a body
over the server's limit, 415 for an unsupported encoding, 422 for a body that
failed validation. They all mean the same thing to a caller: fix the request.
A redirect the client did not follow is `ErrBadRequest` too — the URL is
wrong.

`*decide.Error` also exposes `StatusCode`, `Status`, `Method`, `URL`, `Body`,
`RequestID` and `RetryAfter` for logging. `Body` holds the start of the
response body, up to 4 KiB. `RequestID` is the `X-Request-ID` the client
sent, so a failure can be correlated with the server's log; it is empty when
the request never left the client. `RetryAfter` is how long the server asked
you to wait, from its `Retry-After` header — the client honours it on its
own retries, and it is there for a caller that turned retries off. Use
`errors.Is` to tell success from failure — `StatusCode` is the status on the
wire, so it can be a 2xx on an error when the response arrived but could not
be read.

## Retries

By default the client retries 429/500/502/503/504/529 and transport errors
with exponential backoff and ±20% jitter, capped at 5 s, for up to 4 attempts
(initial + 3 retries). 529 is the API's own overload status — as transient
as a 503. Tune via options:

```go
client := decide.New(baseURL,
    decide.WithRetries(5),                              // up to 6 attempts
    decide.WithBackoff(100*time.Millisecond, 2*time.Second),
)
```

Pass `WithRetries(0)` to disable retries entirely. Context cancellation aborts
the loop immediately; the client never retries on a cancelled context.

When the server sends a `Retry-After` header, the client waits that long
instead of its own backoff, so a rate limit that clears in a few seconds is
not turned into a failure. Waits longer than 30 s are capped at 30 s, so one
request can never block for minutes; set a context deadline if you need a
different bound.

A single call can take a long time when the server keeps asking you to wait:
each attempt has a 60 s timeout and each wait between attempts is up to 30 s,
so the default 4 attempts can run for about five and a half minutes. Set a
context deadline if that is too long.

## Logging

Pass any `*slog.Logger` to a logger; it satisfies the `Logger` interface
directly:

```go
client := decide.New(baseURL,
    decide.WithLogger(slog.Default()),
)
```

Logs include `endpoint`, `request_id`, `attempt`, and `delay` keys. `nil` is
replaced with a discard logger.

## Tokens

The token is sent as an `Authorization: Bearer` header. A token containing a
character that cannot go in an HTTP header — a trailing newline from an
environment variable, say — is rejected before anything is sent, and reported
as `ErrBadRequest`. Trim it when you load it:

```go
token := strings.TrimSpace(os.Getenv("DECIDE_TOKEN"))
client := decide.New(baseURL, decide.WithToken(token))
```

## Connections

The default client keeps up to 100 idle connections to the API host, so
concurrent calls reuse connections instead of opening a new TCP connection
(and a new TLS handshake) each time. Go's stock transport keeps only 2 per
host, which is too few for a client that talks to a single endpoint.

Pass your own `*http.Client` with `WithHTTPClient` if you need different
pool, proxy, or timeout settings.

The client does not follow a redirect to a different host. Go re-sends the
request body on a 307/308, and the body is your state — a customer's email, a
ticket — so the text stays on the server you chose. A redirect to the same
host and port is followed, so an http-to-https upgrade still works. A refused
redirect comes back as an error with a 3xx `StatusCode`.

## Running against a local model

Ollama serves the same `/v1/systemone` endpoint, so this client talks to it
directly. Ollama requires a `model` in the body; servers that route requests
themselves want none.

```bash
ollama pull laya
```

```go
client := decide.New("http://localhost:11434",
    decide.WithToken("ollama-ignores-this"),
    decide.WithModel("laya"),
)
```

Set `WithModel` for Ollama and for Laya servers. Leave it unset for servers
that route the request themselves — the hosted API treats a checkpoint name it
does not know as "let the router choose", and a name that looks like a path
is rejected.

A model on the request overrides the client's default:

```go
resp, err := client.SystemOne(ctx, decide.Request{
    Model:  "other-checkpoint",
    State:  decide.State{Body: "I was charged twice."},
    Questions: decide.Questions{"department": decide.NewChoice("Which team?", opts)},
})
```

### What the state can carry

The API accepts a bare string or an object of any shape. This client sends an
object with `body`, and optionally `subject` and `from` for mail triage:

```go
decide.State{
    From:    "customer@acme.com",
    Subject: "Duplicate billing on invoice #4411",
    Body:    "We were billed twice for March.",
}
```

The model reads the body on its own. The subject matters whenever the body does
not carry the signal, whatever the body's length: on a body that says nothing
about the intent, adding the subject moved the answer from a 0.13-confidence
guess to 0.96. On a body that already says it, the subject changes little.

### Which checkpoint answered

`Response.Routing` says which checkpoint answered and why, when the server
reports it. It matters when the server has more than one: the English
checkpoint reads Latin script only, so a request in another script can be
answered by a checkpoint that cannot read it. Servers that use a single
checkpoint do not report routing, and `Routing.Model` is then empty.

## Request IDs

The client generates a UUID v4 per request and sends it as `X-Request-ID`. The
response's `RequestID()` returns the value the server echoed back, or the one
the client sent if the server did not echo it — so you always get an ID you
can correlate on.

Propagate an upstream ID from your caller's context:

```go
ctx = decide.WithContextRequestID(ctx, upstreamReqID())
resp, err := client.SystemOne(ctx, req)
// resp.RequestID() == upstreamReqID()
```

An ID containing a character that cannot go in an HTTP header (a newline,
say) is rejected before anything is sent, and reported as `ErrBadRequest`.

Or supply your own generator:

```go
client := decide.New(baseURL,
    decide.WithRequestIDFunc(func() string { return traceID() }),
)
```

## Example exchange

Request:

```json
POST /v1/systemone
Authorization: Bearer <TOKEN>
Content-Type: application/json
X-Request-ID: 81f778ef-133d-41c9-99f6-f47417675689

{
  "state": {"body": "I was charged twice, I want a refund."},
  "questions": {
    "department": {
      "type": "choice",
      "instructions": "Which department should handle this?",
      "criteria": {
        "billing":   "invoices, payments, refunds",
        "technical": "bugs and outages",
        "other":     "everything else"
      }
    },
    "urgency": {
      "type": "score",
      "instructions": "How urgent is this?",
      "criteria": ["not urgent", "soon", "blocking"]
    },
    "refund_requested": {
      "type": "noul",
      "instructions": "Does the user request a refund?"
    }
  }
}
```

Response:

```json
{
  "model": "laya-rl-agent",
  "answers": {
    "department": {
      "type": "choice",
      "choice": "billing",
      "probabilities": {"billing": 0.9998, "technical": 0.0, "other": 0.0001},
      "confidence": 0.9983,
      "answer_confidence": 0.9998,
      "action": {"act_probability": 1.0}
    },
    "urgency": {
      "type": "score",
      "score": 1.84,
      "legend": {"0": "not urgent", "1": "soon", "2": "blocking"},
      "probabilities": {"0": 0.02, "1": 0.12, "2": 0.86},
      "confidence": 0.83,
      "answer_confidence": 0.86,
      "action": {"act_probability": 1.0}
    },
    "refund_requested": {
      "type": "noul",
      "noul": 0.993,
      "confidence": 0.993,
      "answer_confidence": 0.993,
      "action": {"act_probability": 1.0}
    }
  },
  "routing": {
    "model": "english",
    "repo": "convaiinnovations/laya",
    "reason": "Latin script; the English checkpoint reads it"
  },
  "usage": {
    "input_tokens": 105, "output_tokens": 0, "state_tokens": 20,
    "state_tokens_dropped": 0, "truncated": false, "truncated_questions": []
  }
}
```

## API surface

```go
// Construction
New(baseURL string, opts ...Option) *Client
WithToken(string) Option
WithModel(string) Option
WithLongContext() Option
WithHTTPClient(*http.Client) Option
WithRetries(int) Option
WithBackoff(initial, max time.Duration) Option
WithLogger(Logger) Option
WithRequestIDFunc(func() string) Option

// Types
State       { Body, Subject, From string }
Question    { Type, Instructions string; Criteria map[string]string; Levels []string }
NewChoice(instructions string, criteria map[string]string) Question
NewScore(instructions string, levels []string) Question
NewNoul(instructions string) Question
Questions   map[string]Question
Request     { Model, State, Questions }

Response    { Model, Answers map[string]Answer, Usage, Routing; RequestID() }
Answer      { Type, Choice, Probabilities, Noul, Score, Legend,
              Confidence, AnswerConfidence, Action;
              IsChoice(), IsNoul(), IsScore(),
              ChoiceKey(), NoulValue(), ScoreValue(), ScoreLegend() }
Action      { ActProbability }
Usage       { InputTokens, OutputTokens, StateTokens, StateTokensDropped,
              Truncated, TruncatedQuestions }
Routing     { Model, Repo, Reason }

// Sentinels (match with errors.Is)
ErrBadRequest, ErrUnauthorized, ErrForbidden, ErrNotFound,
ErrRateLimited, ErrServerError, ErrTransport, ErrResponseDecoded,
ErrResponseTooLarge

// Context
WithContextRequestID(ctx, id) context.Context
RequestIDFromContext(ctx) string

// Errors
*Error      { StatusCode, Status, Method, URL, Body, RequestID, RetryAfter;
              IsRetryable(); Unwrap() }
```

## Testing

```
go test -race -count=1 ./...
```

Coverage runs at ~99% of statements; the package has no transitive dependencies.

There is also a test that talks to a real decision server. It is skipped
unless you point it at one:

```
DECIDE_TEST_URL=http://localhost:11434 DECIDE_TEST_MODEL=laya go test -run TestLive
```

## Releasing

The project follows [Conventional Commits][cc] for changelog generation and
version bumping:

- `feat:` triggers a minor bump
- `fix:` triggers a patch bump
- `feat!` or a `BREAKING CHANGE:` footer triggers a major bump
- Other types (`ci:`, `docs:`, `refactor:`, etc.) don't trigger a bump but
  may appear in the changelog under "Others"

[cc]: https://www.conventionalcommits.org/

### Cutting a release

```bash
# Decide the next version from the commits since the last tag, then:
git tag v0.2.0
git push origin v0.2.0
```

Pushing the tag triggers `.github/workflows/release.yml`, which runs
[GoReleaser][gr] and produces a draft GitHub release with a generated
changelog. Review and publish from the GitHub UI.

[gr]: https://goreleaser.com/

### Local dry run

```bash
goreleaser release --snapshot --clean
```

Runs the full release pipeline locally without pushing or creating a release.
Useful for validating `.goreleaser.yaml` changes.

### CI

`.github/workflows/ci.yml` runs `go mod tidy`, `go vet`, `go test -race
-cover`, and `golangci-lint` on every push and PR to `main`.