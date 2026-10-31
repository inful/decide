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

func TestResponse_RequestIDOnNilResponse(t *testing.T) {
	// RequestID is documented as safe on a nil receiver.
	var r *Response
	if got := r.RequestID(); got != "" {
		t.Errorf("RequestID() on a nil Response = %q, want %q", got, "")
	}
}

func TestAnswer_ScoreType(t *testing.T) {
	payload := `{
		"type": "score",
		"score": 1.5757,
		"legend": {"0": "Not urgent", "1": "Soon", "2": "Critical"},
		"probabilities": {"0": 0.0812, "1": 0.2618, "2": 0.657},
		"confidence": 0.2437
	}`

	var a Answer
	if err := json.Unmarshal([]byte(payload), &a); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !a.IsScore() {
		t.Fatalf("expected score type, got %q", a.Type)
	}
	if !floatEq(a.ScoreValue(), 1.5757) {
		t.Errorf("score = %v, want 1.5757", a.ScoreValue())
	}
	legend := a.ScoreLegend()
	if len(legend) != 3 || legend["2"] != "Critical" {
		t.Errorf("legend = %#v", legend)
	}
	if a.Probabilities["2"] < 0.65 || a.Probabilities["2"] > 0.66 {
		t.Errorf("p[2] = %v, want about 0.657", a.Probabilities["2"])
	}
}

func TestAnswer_ScoreAccessorsAreSafeOnOtherTypes(t *testing.T) {
	// A non-score answer must not leak a score or a legend.
	noul := Answer{Type: "noul", Noul: 0.4, Score: 3, Legend: map[string]string{"0": "x"}}
	if noul.IsScore() {
		t.Errorf("noul.IsScore should be false")
	}
	if got := noul.ScoreValue(); got != 0 {
		t.Errorf("ScoreValue() = %v, want 0", got)
	}
	if got := noul.ScoreLegend(); got != nil {
		t.Errorf("ScoreLegend() = %#v, want nil", got)
	}
}

func TestResponseRouting(t *testing.T) {
	payload := `{
		"model": "laya-rl-agent",
		"answers": {},
		"usage": {},
		"routing": {
			"model": "english",
			"repo": "convaiinnovations/laya",
			"reason": "English Latin text"
		}
	}`

	var r Response
	if err := json.Unmarshal([]byte(payload), &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if r.Routing.Model != "english" {
		t.Errorf("routing.model = %q, want %q", r.Routing.Model, "english")
	}
	if r.Routing.Repo != "convaiinnovations/laya" {
		t.Errorf("routing.repo = %q", r.Routing.Repo)
	}
	if r.Routing.Reason != "English Latin text" {
		t.Errorf("routing.reason = %q", r.Routing.Reason)
	}
}

func TestResponseRoutingEmptyWhenNotReported(t *testing.T) {
	var r Response
	if err := json.Unmarshal([]byte(`{"model":"laya"}`), &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if r.Routing.Model != "" {
		t.Errorf("routing.model = %q, want empty when the server does not report it", r.Routing.Model)
	}
}

func floatEq(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}
