package decide

// Response is the parsed body returned by /v1/systemone.
type Response struct {
	Model   string            `json:"model"`
	Answers map[string]Answer `json:"answers"`
	Usage   Usage             `json:"usage"`
	Routing Routing           `json:"routing,omitempty"`

	// requestID is captured from the X-Request-ID response header, if present.
	requestID string `json:"-"`
}

// Routing says which checkpoint answered and why. Servers that route the
// request themselves send it; a server that always uses one checkpoint does
// not, and Model is then empty.
//
// It matters when the server has more than one checkpoint: the English one
// reads Latin script only, so a request in another script can be answered by
// a checkpoint that cannot read it.
type Routing struct {
	// Model is the checkpoint that answered, for example "english".
	Model string `json:"model"`
	// Repo is the model repository it came from, for example
	// "convaiinnovations/laya".
	Repo string `json:"repo"`
	// Reason explains why that checkpoint was chosen.
	Reason string `json:"reason"`
}

// RequestID returns the X-Request-ID echoed by the server. When the server
// does not echo it, this is the ID the client sent, so the caller always
// gets the ID it can correlate on.
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
//	"score"  → Score, Legend, Probabilities
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
	Score            float64            `json:"score,omitempty"`
	Legend           map[string]string  `json:"legend,omitempty"`
	Confidence       float64            `json:"confidence"`
	AnswerConfidence float64            `json:"answer_confidence"`
	Action           Action             `json:"action"`
}

// IsChoice reports whether this answer is for a "choice" question.
func (a Answer) IsChoice() bool { return a.Type == "choice" }

// IsNoul reports whether this answer is for a "noul" question.
func (a Answer) IsNoul() bool { return a.Type == "noul" }

// IsScore reports whether this answer is for a "score" question.
func (a Answer) IsScore() bool { return a.Type == "score" }

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

// ScoreValue returns the expected level for a "score" answer, or 0 for any
// other type. It is the probability-weighted level, so it can fall between
// two levels.
func (a Answer) ScoreValue() float64 {
	if !a.IsScore() {
		return 0
	}
	return a.Score
}

// ScoreLegend returns the level descriptions of a "score" answer, keyed by
// level index, or nil for any other type.
func (a Answer) ScoreLegend() map[string]string {
	if !a.IsScore() {
		return nil
	}
	return a.Legend
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
