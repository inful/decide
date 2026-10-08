package decide

// Response is the parsed body returned by /v1/systemone.
type Response struct {
	Model   string            `json:"model"`
	Answers map[string]Answer `json:"answers"`
	Usage   Usage             `json:"usage"`

	// requestID is captured from the X-Request-ID response header, if present.
	requestID string `json:"-"`
}

// RequestID returns the X-Request-ID echoed (or freshly minted) by the server.
// Empty when the server did not include the header.
func (r *Response) RequestID() string {
	if r == nil {
		return ""
	}
	return r.requestID
}

// Answer is one answered question. The Type field discriminates which fields
// are populated:
//
//	"choice" → Choice, Probabilities
//	"noul"   → Noul
//
// Confidence, AnswerConfidence, and Action are always present. Use the Is… and
// …Value helpers rather than checking fields directly so that accessors don't
// leak across types.
type Answer struct {
	Type             string             `json:"type"`
	Choice           string             `json:"choice,omitempty"`
	Probabilities    map[string]float64 `json:"probabilities,omitempty"`
	Noul             float64            `json:"noul,omitempty"`
	Confidence       float64            `json:"confidence"`
	AnswerConfidence float64            `json:"answer_confidence"`
	Action           Action             `json:"action"`
}

// IsChoice reports whether this answer is for a "choice" question.
func (a Answer) IsChoice() bool { return a.Type == "choice" }

// IsNoul reports whether this answer is for a "noul" question.
func (a Answer) IsNoul() bool { return a.Type == "noul" }

// ChoiceKey returns the selected criterion key for a "choice" answer, or ""
// for any other type. Safe to call on a zero Answer.
func (a Answer) ChoiceKey() string {
	if !a.IsChoice() {
		return ""
	}
	return a.Choice
}

// NoulValue returns the [0, 1] probability for a "noul" answer, or 0 for any
// other type.
func (a Answer) NoulValue() float64 {
	if !a.IsNoul() {
		return 0
	}
	return a.Noul
}

// Action describes the model's confidence that it should act on this answer.
type Action struct {
	ActProbability float64 `json:"act_probability"`
}

// Usage reports token accounting for the request, as returned by the API.
type Usage struct {
	InputTokens        int      `json:"input_tokens"`
	OutputTokens       int      `json:"output_tokens"`
	StateTokens        int      `json:"state_tokens"`
	StateTokensDropped int      `json:"state_tokens_dropped"`
	Truncated          bool     `json:"truncated"`
	TruncatedQuestions []string `json:"truncated_questions"`
}