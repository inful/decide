# decide

A small, dependency-free Go client for the `/v1/systemone` decision API.

The API accepts unstructured context (`state.body`) and a map of structured
questions (`questions.<key>`), and answers each question according to its type
and instructions. See the [example exchange](#example-exchange) below.

## Install

```
go get github.com/inful/decide
```

Go 1.22+ is sufficient; the module declares Go 1.26.

## Quick start

```go
client := decide.New(
    "https://api.example.com",
    decide.WithToken("sk-gw-..."),
)

resp, err := client.SystemOne(ctx, decide.Request{
    State: decide.State{Body: "Jeg ble fakturert to ganger, jeg vil ha pengene tilbake."},
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

Only two types are modeled today:

| Type    | Constructor                       | Response fields           |
|---------|-----------------------------------|---------------------------|
| choice  | `NewChoice(instr, criteria)`      | `Choice`, `Probabilities` |
| noul    | `NewNoul(instr)`                  | `Noul` (a `[0,1]` prob)   |

Both response types also carry:

- `Confidence`, `AnswerConfidence` — model self-reported confidence
- `Action.ActProbability` — how strongly the model thinks it should act on this answer

All questions share `Confidence`, `AnswerConfidence`, and `Action` regardless of type.

## Errors

`Client.SystemOne` returns either a `*decide.Response` or a `*decide.Error`. Use
`errors.Is` to branch on category:

```go
_, err := client.SystemOne(ctx, req)
switch {
case errors.Is(err, decide.ErrBadRequest):     // 400 — fix the request
case errors.Is(err, decide.ErrUnauthorized):   // 401 — check the token
case errors.Is(err, decide.ErrRateLimited):    // 429 — back off (already retried)
case errors.Is(err, decide.ErrServerError):    // 5xx — back off (already retried)
case errors.Is(err, decide.ErrTransport):      // network/dial/TLS (already retried)
case errors.Is(err, decide.ErrResponseDecoded):// unexpected payload shape
case err != nil:
    return err
}
```

`*decide.Error` also exposes `StatusCode`, `Status`, `Method`, `URL`, and `Body`
for logging.

## Retries

By default the client retries 429/500/502/503/504 and transport errors with
exponential backoff and ±20% jitter, capped at 5 s, for up to 4 attempts
(initial + 3 retries). Tune via options:

```go
client := decide.New(baseURL,
    decide.WithRetries(5),                              // up to 6 attempts
    decide.WithBackoff(100*time.Millisecond, 2*time.Second),
)
```

Pass `WithRetries(0)` to disable retries entirely. Context cancellation aborts
the loop immediately; the client never retries on a cancelled context.

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

## Request IDs

The client generates a UUID v4 per request and sends it as `X-Request-ID`. The
response's `RequestID()` returns the value the server echoed back (or the
generated one, if the server didn't echo).

Propagate an upstream ID from your caller's context:

```go
ctx = decide.WithContextRequestID(ctx, upstreamReqID())
resp, err := client.SystemOne(ctx, req)
// resp.RequestID() == upstreamReqID()
```

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
Authorization: Bearer sk-gw-...
Content-Type: application/json
X-Request-ID: 81f778ef-133d-41c9-99f6-f47417675689

{
  "state": {"body": "Jeg ble fakturert to ganger, jeg vil ha pengene tilbake."},
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
    "refund_requested": {
      "type": "noul",
      "noul": 0.993,
      "confidence": 0.993,
      "answer_confidence": 0.993,
      "action": {"act_probability": 1.0}
    }
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
WithHTTPClient(*http.Client) Option
WithRetries(int) Option
WithBackoff(initial, max time.Duration) Option
WithLogger(Logger) Option
WithRequestIDFunc(func() string) Option

// Types
State       { Body string }
Question    { Type, Instructions string; Criteria map[string]string }
NewChoice(instructions string, criteria map[string]string) Question
NewNoul(instructions string) Question
Questions   map[string]Question
Request     { State, Questions }

Response    { Model, Answers map[string]Answer, Usage; RequestID() }
Answer      { Type, Choice, Probabilities, Noul, Confidence, AnswerConfidence, Action;
              IsChoice(), IsNoul(), ChoiceKey(), NoulValue() }
Action      { ActProbability }
Usage       { InputTokens, OutputTokens, StateTokens, StateTokensDropped,
              Truncated, TruncatedQuestions }

// Sentinels (match with errors.Is)
ErrBadRequest, ErrUnauthorized, ErrForbidden, ErrNotFound,
ErrRateLimited, ErrServerError, ErrTransport, ErrResponseDecoded

// Context
WithContextRequestID(ctx, id) context.Context
RequestIDFromContext(ctx) string

// Errors
*Error      { StatusCode, Status, Method, URL, Body; IsRetryable(); Unwrap() }
```

## Testing

```
go test -race -count=1 ./...
```

Coverage runs at ~90% of statements; the package has no transitive dependencies.

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