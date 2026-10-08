package decide

// State is the unstructured context the model inspects. Today the API
// accepts only a Body; future fields (e.g. conversation history) can be added
// here without breaking callers.
type State struct {
	Body string `json:"body"`
}

// Question is a single structured question asked of the model. The Type field
// discriminates the shape:
//
//	"choice" — pick one key from Criteria (string keys, string descriptions).
//	"noul"   — a "nullable bool" the API answers with a probability in [0, 1].
//
// Use the NewChoice / NewNoul constructors for type-safe construction.
type Question struct {
	Type         string            `json:"type"`
	Instructions string            `json:"instructions"`
	Criteria     map[string]string `json:"criteria,omitempty"`
}

// NewChoice builds a "choice"-typed Question.
func NewChoice(instructions string, criteria map[string]string) Question {
	return Question{Type: "choice", Instructions: instructions, Criteria: criteria}
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
	State     State     `json:"state"`
	Questions Questions `json:"questions"`
}