package decide

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestNewChoiceMarshal(t *testing.T) {
	criteria := map[string]string{
		"billing":   "invoices, payments, refunds",
		"technical": "bugs and outages",
		"other":     "everything else",
	}
	q := NewChoice("Which department should handle this?", criteria)

	data, err := json.Marshal(q)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Map keys have no canonical order; assert structurally by decoding back.
	var back Question
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Type != "choice" {
		t.Errorf("type = %q, want choice", back.Type)
	}
	if back.Instructions != "Which department should handle this?" {
		t.Errorf("instructions = %q", back.Instructions)
	}
	if !reflect.DeepEqual(back.Criteria, criteria) {
		t.Errorf("criteria = %#v, want %#v", back.Criteria, criteria)
	}
}

func TestNewNoulMarshalOmitsCriteria(t *testing.T) {
	q := NewNoul("Does the user request a refund?")

	data, err := json.Marshal(q)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	want := `{"type":"noul","instructions":"Does the user request a refund?"}`
	if string(data) != want {
		t.Errorf("got %s\nwant %s", data, want)
	}
}

func TestRequestRoundTripMatchesCurlExample(t *testing.T) {
	original := Request{
		State: State{Body: "I was charged twice, I want a refund."},
		Questions: Questions{
			"department": NewChoice(
				"Which department should handle this?",
				map[string]string{
					"billing":   "invoices, payments, refunds",
					"technical": "bugs and outages",
					"other":     "everything else",
				},
			),
			"refund_requested": NewNoul("Does the user request a refund?"),
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got Request
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if !reflect.DeepEqual(got, original) {
		t.Errorf("round trip mismatch\noriginal: %#v\ngot:      %#v\njson:    %s", original, got, data)
	}

	for _, want := range []string{
		`"state":{"body":"I was charged twice, I want a refund."}`,
		`"department"`,
		`"refund_requested"`,
		`"type":"choice"`,
		`"type":"noul"`,
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf("payload missing %q: %s", want, data)
		}
	}
}

func TestQuestionsUnmarshalUnknownTypeIsPreserved(t *testing.T) {
	// If the API ever adds a new question type, we shouldn't drop it on the
	// request side; callers may pass through types we don't yet model.
	payload := `{
		"state": {"body": "hi"},
		"questions": {
			"mood": {"type": "future_thing", "instructions": "how are they feeling?"}
		}
	}`

	var req Request
	if err := json.Unmarshal([]byte(payload), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	q, ok := req.Questions["mood"]
	if !ok {
		t.Fatalf("mood question not present: %#v", req.Questions)
	}
	if q.Type != "future_thing" {
		t.Errorf("type = %q, want future_thing", q.Type)
	}
	if q.Instructions != "how are they feeling?" {
		t.Errorf("instructions = %q", q.Instructions)
	}
}