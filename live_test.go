package decide

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestLiveModel talks to a real decision server — a local Ollama with the
// laya model, for instance. It is skipped unless DECIDE_TEST_URL is set:
//
//	DECIDE_TEST_URL=http://localhost:11434 DECIDE_TEST_MODEL=laya go test -run TestLive
//
// Set DECIDE_TEST_MODEL when the server needs a model name: Ollama answers
// 400 "model is required" without one. Servers that route requests
// themselves do not need it.
//
// The assertions are about shape, not about what the model decides. A model
// is allowed to change its mind; it is not allowed to stop answering in the
// documented format.
func TestLiveModel(t *testing.T) {
	base := os.Getenv("DECIDE_TEST_URL")
	if base == "" {
		t.Skip("set DECIDE_TEST_URL to run against a real server")
	}
	model := os.Getenv("DECIDE_TEST_MODEL")

	opts := []Option{WithToken("live-test")}
	if model != "" {
		opts = append(opts, WithModel(model))
	}
	client := New(base, opts...)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	resp, err := client.SystemOne(ctx, Request{
		State: State{
			From:    "customer@acme.com",
			Subject: "Duplicate billing on March invoice #4411",
			Body:    "Hi team, we were billed twice for March. Please refund the duplicate before Friday or we will cancel our plan.",
		},
		Questions: Questions{
			"department": NewChoice(
				"Which department should handle this email?",
				map[string]string{
					"billing":   "Invoices, payments, refunds",
					"technical": "Bugs, outages, integrations",
					"other":     "Everything else",
				},
			),
			"urgency": NewScore(
				"How urgent is this request?",
				[]string{"Not urgent", "Soon", "Critical deadline or blocking issue"},
			),
			"churn_risk": NewNoul("Does the user threaten to cancel or switch to a competitor?"),
		},
	})
	if err != nil {
		t.Fatalf("SystemOne: %v", err)
	}

	if resp.Model == "" {
		t.Error("the server did not report a model")
	}
	if len(resp.Answers) != 3 {
		t.Fatalf("got %d answers, want 3", len(resp.Answers))
	}

	dept := resp.Answers["department"]
	if !dept.IsChoice() {
		t.Fatalf("department should be a choice answer, got type %q", dept.Type)
	}
	if dept.ChoiceKey() == "" {
		t.Error("department has no chosen option")
	}
	assertProbabilitiesSumToOne(t, "department", dept.Probabilities)

	urgency := resp.Answers["urgency"]
	if !urgency.IsScore() {
		t.Fatalf("urgency should be a score answer, got type %q", urgency.Type)
	}
	if len(urgency.ScoreLegend()) != 3 {
		t.Errorf("urgency legend has %d levels, want 3", len(urgency.ScoreLegend()))
	}
	assertProbabilitiesSumToOne(t, "urgency", urgency.Probabilities)

	churn := resp.Answers["churn_risk"]
	if !churn.IsNoul() {
		t.Fatalf("churn_risk should be a noul answer, got type %q", churn.Type)
	}
	if churn.NoulValue() < 0 || churn.NoulValue() > 1 {
		t.Errorf("noul = %v, want a probability in [0,1]", churn.NoulValue())
	}
}

func assertProbabilitiesSumToOne(t *testing.T, name string, p map[string]float64) {
	t.Helper()
	if len(p) == 0 {
		t.Errorf("%s has no probabilities", name)
		return
	}
	var total float64
	for _, v := range p {
		total += v
	}
	if total < 0.99 || total > 1.01 {
		t.Errorf("%s probabilities sum to %v, want about 1", name, total)
	}
}
