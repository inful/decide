package decide

import (
	"encoding/json"
	"math"
	"testing"
)

// responseFixture is the real response body shape documented for /v1/systemone.
// Pinned here so a server-side change has to update both client and tests.
const responseFixture = `{
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
		"input_tokens": 105,
		"output_tokens": 0,
		"state_tokens": 20,
		"state_tokens_dropped": 0,
		"truncated": false,
		"truncated_questions": []
	}
}`

func TestResponseUnmarshalFromFixture(t *testing.T) {
	var resp Response
	if err := json.Unmarshal([]byte(responseFixture), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if resp.Model != "laya-rl-agent" {
		t.Errorf("model = %q", resp.Model)
	}

	if len(resp.Answers) != 2 {
		t.Fatalf("Answers length = %d, want 2", len(resp.Answers))
	}

	dept := resp.Answers["department"]
	if !dept.IsChoice() {
		t.Errorf("department should be choice, got type=%q", dept.Type)
	}
	if got := dept.ChoiceKey(); got != "billing" {
		t.Errorf("department choice key = %q, want billing", got)
	}
	if !dept.IsChoice() {
		t.Fatalf("expected choice type for department")
	}
	if got := dept.Probabilities["billing"]; !floatEq(got, 0.9998) {
		t.Errorf("department.billing probability = %v", got)
	}
	if got := dept.Probabilities["other"]; !floatEq(got, 0.0001) {
		t.Errorf("department.other probability = %v", got)
	}

	refund := resp.Answers["refund_requested"]
	if !refund.IsNoul() {
		t.Errorf("refund_requested should be noul, got type=%q", refund.Type)
	}
	if got := refund.NoulValue(); !floatEq(got, 0.993) {
		t.Errorf("refund_requested noul value = %v, want 0.993", got)
	}

	if !floatEq(refund.Confidence, 0.993) {
		t.Errorf("refund confidence = %v", refund.Confidence)
	}
	if !floatEq(refund.Action.ActProbability, 1.0) {
		t.Errorf("refund action.act_probability = %v", refund.Action.ActProbability)
	}

	if resp.Usage.InputTokens != 105 {
		t.Errorf("usage.input_tokens = %d", resp.Usage.InputTokens)
	}
	if resp.Usage.OutputTokens != 0 {
		t.Errorf("usage.output_tokens = %d", resp.Usage.OutputTokens)
	}
	if resp.Usage.StateTokens != 20 {
		t.Errorf("usage.state_tokens = %d", resp.Usage.StateTokens)
	}
	if resp.Usage.Truncated {
		t.Errorf("usage.truncated should be false")
	}
}

func TestAnswer_NonDiscriminatorFieldsDontLeakBetweenTypes(t *testing.T) {
	// A noul response should not accidentally expose a ChoiceKey() value
	// even if the field is present-and-zero.
	noul := Answer{Type: "noul", Noul: 0.4}
	if noul.IsChoice() {
		t.Errorf("noul.IsChoice should be false")
	}
	if got := noul.ChoiceKey(); got != "" {
		t.Errorf("noul.ChoiceKey() = %q, want empty", got)
	}

	choice := Answer{Type: "choice", Choice: "billing"}
	if choice.IsNoul() {
		t.Errorf("choice.IsNoul should be false")
	}
	if got := choice.NoulValue(); got != 0 {
		t.Errorf("choice.NoulValue() = %v, want 0", got)
	}
}

func floatEq(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}