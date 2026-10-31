package decide

import "encoding/json"

// State is the unstructured context the model inspects.
//
// The API accepts a bare string or an object of any shape. This client sends
// an object. Body is what the model reads; Subject and From only matter for
// mail triage, where the subject carries signal the body does not.
type State struct {
	Body    string `json:"body"`
	Subject string `json:"subject,omitempty"`
	From    string `json:"from,omitempty"`
}

// Question is a single structured question asked of the model. The Type field
// discriminates the shape:
//
//	"choice" — pick one key from Criteria (string keys, string descriptions).
//	"score"  — place the state on an ordered scale; Levels holds the levels.
//	"noul"   — a "nullable bool" the API answers with a probability in [0, 1].
//
// Use the NewChoice / NewScore / NewNoul constructors for type-safe
// construction.
type Question struct {
	Type         string            `json:"type"`
	Instructions string            `json:"instructions"`
	Criteria     map[string]string `json:"criteria,omitempty"`

	// Levels holds the ordered level descriptions of a "score" question,
	// lowest first. It is sent as the "criteria" field; Criteria is used
	// for every other type.
	Levels []string `json:"-"`
}

// questionWire is the wire shape of a Question. The "criteria" field is an
// object for a choice question and an array for a score question, so it
// cannot be a single typed field.
type questionWire struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

// MarshalJSON sends the levels of a score question as the "criteria" array,
// and the criteria map for every other type.
func (q Question) MarshalJSON() ([]byte, error) {
	w := questionWire{Type: q.Type, Instructions: q.Instructions}
	switch {
	case len(q.Levels) > 0:
		w.Criteria = q.Levels
	case len(q.Criteria) > 0:
		w.Criteria = q.Criteria
	}
	return json.Marshal(w)
}

// UnmarshalJSON reads a "criteria" array into Levels and a "criteria" object
// into Criteria.
func (q *Question) UnmarshalJSON(b []byte) error {
	var w struct {
		Type         string          `json:"type"`
		Instructions string          `json:"instructions"`
		Criteria     json.RawMessage `json:"criteria"`
	}
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	q.Type = w.Type
	q.Instructions = w.Instructions
	q.Criteria = nil
	q.Levels = nil
	if len(w.Criteria) == 0 {
		return nil
	}
	if w.Criteria[0] == '[' {
		return json.Unmarshal(w.Criteria, &q.Levels)
	}
	return json.Unmarshal(w.Criteria, &q.Criteria)
}

// NewChoice builds a "choice"-typed Question.
func NewChoice(instructions string, criteria map[string]string) Question {
	return Question{Type: "choice", Instructions: instructions, Criteria: criteria}
}

// NewScore builds a "score"-typed Question. The levels are ordered from
// lowest to highest; the API answers with the expected level and the
// probability of each level.
func NewScore(instructions string, levels []string) Question {
	return Question{Type: "score", Instructions: instructions, Levels: levels}
}

// NewNoul builds a "noul"-typed Question (the API returns a probability, not a
// strict boolean).
func NewNoul(instructions string) Question {
	return Question{Type: "noul", Instructions: instructions}
}

// Questions is a map from caller-chosen question key to Question. Keys are
// used both to look up answers in the Response and to group questions in logs.
type Questions map[string]Question

// Request is the body posted to /v1/systemone.
type Request struct {
	// Model names the checkpoint that should answer. Leave it empty to let
	// the server pick one. The client's WithModel option sets the default.
	Model     string    `json:"model,omitempty"`
	State     State     `json:"state"`
	Questions Questions `json:"questions"`
}
