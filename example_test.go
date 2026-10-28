package decide_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/inful/decide"
)

// ExampleClient_SystemOne shows the usual round trip: build a request with a
// choice question and a noul question, call the API, and read the answers.
//
// The httptest server stands in for the real API. Point decide.New at your
// own host and drop the server.
func ExampleClient_SystemOne() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
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
			"usage": {"input_tokens": 105, "output_tokens": 0}
		}`))
	}))
	defer srv.Close()

	client := decide.New(srv.URL,
		decide.WithToken("example-token"),
		// Keep the output of this example stable.
		decide.WithRequestIDFunc(func() string { return "example-request-id" }),
	)

	resp, err := client.SystemOne(context.Background(), decide.Request{
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
		fmt.Println("call failed:", err)
		return
	}

	fmt.Println("department:", resp.Answers["department"].ChoiceKey())
	fmt.Println("refund requested:", resp.Answers["refund_requested"].NoulValue())
	fmt.Println("request id:", resp.RequestID())

	// Output:
	// department: billing
	// refund requested: 0.993
	// request id: example-request-id
}
