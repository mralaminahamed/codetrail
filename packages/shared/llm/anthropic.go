package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// anthropicVersion is mandatory on every Messages API request; a request
// without it is rejected. It is a compiled-in constant rather than a knob, and
// because this client deliberately ships NO BOOT PROBE, its absence would first
// surface on a caller's paid request in production rather than at startup.
const anthropicVersion = "2023-06-01"

const messagesPath = "/v1/messages"

// Anthropic is a hand-rolled client over net/http.
//
// Not an SDK, and the reason is not weight: every control in this phase's
// egress resolution is a property of the transport, and an SDK owns the
// transport. Proxy: nil is an *http.Transport field; refusing every redirect is
// an *http.Client field; and an SDK that retries a 429 three times of its own
// accord spends the loop's one deadline on a schedule the trace never sees.
//
// packages/shared/embed/ollama.go is the shipped precedent for exactly this
// shape: NewRequestWithContext, a status check before the decode with a bounded
// body snippet in the error, and post-conditions on the response.
type Anthropic struct {
	baseURL string
	model   string
	key     Secret
	client  *http.Client
}

// NewAnthropic builds the client.
//
// It accepts an ARBITRARY baseURL, deliberately, so the hermetic tests can point
// it at an httptest.Server. The host allowlist therefore lives in FromEnv and
// not here, which means the egress claim is exactly as strong as "FromEnv is the
// only non-test caller of NewAnthropic" — asserted by a test that greps the
// non-test tree, not assumed. Putting the allowlist in the constructor and
// giving tests a separate unexported constructor was considered and refused: it
// moves the same hole one identifier over and hides the test seam.
func NewAnthropic(baseURL, model string, key Secret, timeout time.Duration) (*Anthropic, error) {
	if strings.TrimSpace(model) == "" {
		return nil, errors.New("llm: model must not be empty")
	}
	if key.empty() {
		return nil, errors.New("llm: api key must not be empty")
	}
	return &Anthropic{
		baseURL: strings.TrimRight(baseURL, "/"),
		model:   model,
		key:     key,
		client: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				// Written out rather than omitted. http.DefaultTransport uses
				// http.ProxyFromEnvironment, so an operator's HTTPS_PROXY — or
				// one inherited into a container by accident — would silently
				// redirect a credential-bearing request. P4 recorded that the
				// settings closed BY OMISSION are the ones a mutation can be
				// built against.
				Proxy:               nil,
				ForceAttemptHTTP2:   true,
				TLSHandshakeTimeout: 10 * time.Second,
			},
			// Every redirect, not only a cross-host one. Go strips exactly six
			// headers on a cross-domain redirect — Authorization,
			// Www-Authenticate, Cookie, Cookie2, Proxy-Authorization,
			// Proxy-Authenticate — and x-api-key is on none of them, so Go
			// forwards this client's credential to whatever host a redirect
			// names. A control that reasoned about Authorization would protect
			// a header this client never sends.
			CheckRedirect: func(req *http.Request, _ []*http.Request) error {
				return fmt.Errorf("llm: refusing a redirect to %s", req.URL.Host)
			},
		},
	}, nil
}

func (a *Anthropic) Name() string { return a.model }

// String and GoString exist because Secret's three methods are not enough —
// measured, not assumed. fmt reaches an unexported field by reflection, where
// reflect.Value.CanInterface() is false and the Stringer is unreachable, so
// %+v on this struct would print the key even though %v on the Secret alone
// prints [redacted].
//
// Value receivers, so a *Anthropic and an Anthropic are both covered.
//
// Exporting the field so the Stringer becomes reachable is the alternative and
// is refused: an exported Key invites c.Key at a call site, which is the
// accessor this type exists to withhold.
func (a Anthropic) String() string {
	return "llm.Anthropic{" + a.baseURL + " " + a.model + " " + redacted + "}"
}

func (a Anthropic) GoString() string { return a.String() }

// ---- the wire ------------------------------------------------------------

type wireTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type wireBlock struct {
	Type string `json:"type"`
	// text
	Text string `json:"text,omitempty"`
	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
}

type wireMessage struct {
	Role    string      `json:"role"`
	Content []wireBlock `json:"content"`
}

type wireRequest struct {
	Model     string        `json:"model"`
	MaxTokens int           `json:"max_tokens"`
	System    string        `json:"system,omitempty"`
	Messages  []wireMessage `json:"messages"`
	Tools     []wireTool    `json:"tools,omitempty"`
}

type wireResponse struct {
	Content    []wireBlock `json:"content"`
	StopReason string      `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

func (a *Anthropic) Complete(ctx context.Context, r Request) (Response, error) {
	body, err := json.Marshal(wireRequest{
		Model:     a.model,
		MaxTokens: r.MaxTokens,
		System:    r.System,
		Messages:  wireMessages(r.Messages),
		Tools:     wireTools(r.Tools),
	})
	if err != nil {
		return Response{}, &Failure{Kind: KindMalformed, Detail: "encoding the request: " + err.Error()}
	}
	url := a.baseURL + messagesPath
	// WithContext, so the loop's one deadline reaches the model. Without it a
	// hung generation outlives the loop that owns it and only the client's own
	// Timeout — a different and longer number — would ever end the call.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return Response{}, &Failure{Kind: KindMalformed, Detail: "building the request: " + err.Error()}
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", a.key.reveal())
	req.Header.Set("anthropic-version", anthropicVersion)

	resp, err := a.client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return Response{}, &Failure{Kind: KindDeadline, Detail: err.Error()}
		}
		// A refused connection, a DNS failure, a TLS failure and a refused
		// redirect all land here. None of them carries the key: it is a header,
		// and url.Error names only the method and the URL.
		return Response{}, &Failure{Kind: KindUnavailable, Detail: err.Error()}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Before the decode, not after. Measured one package over: a 404 for a
		// model that was never pulled has a JSON body that decodes cleanly into
		// the success struct, and the failure then surfaces as "the model said
		// nothing" rather than "the provider refused".
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxDetail))
		return Response{}, &Failure{
			Kind:   kindOf(resp.StatusCode),
			Status: resp.StatusCode,
			Detail: strings.TrimSpace(string(snippet)),
		}
	}

	var out wireResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Response{}, &Failure{Kind: KindMalformed, Status: resp.StatusCode, Detail: "decode: " + err.Error()}
	}

	got := Response{Stop: out.StopReason}
	for _, b := range out.Content {
		switch b.Type {
		case "text":
			got.Text += b.Text
		case "tool_use":
			// A tool_use block with no name or no id is not a Response: the loop
			// could neither dispatch it nor answer it, and letting it through
			// would surface as a malformed tool call, which blames the model for
			// a broken provider.
			if b.Name == "" || b.ID == "" {
				return Response{}, &Failure{Kind: KindMalformed, Status: resp.StatusCode,
					Detail: "a tool_use block carried no name or no id"}
			}
			got.Calls = append(got.Calls, ToolCall{ID: b.ID, Name: b.Name, Args: b.Input})
		}
	}
	if len(out.Content) == 0 {
		return Response{}, &Failure{Kind: KindMalformed, Status: resp.StatusCode, Detail: "no content blocks"}
	}

	got.Usage = Usage{InputTokens: out.Usage.InputTokens, OutputTokens: out.Usage.OutputTokens}
	if got.Usage.InputTokens == 0 && got.Usage.OutputTokens == 0 {
		// The provider reported nothing, so the budget spends a guess and says
		// so. A budget spent against an unflagged estimate is one nobody can
		// reconcile with a bill.
		got.Usage = EstimatedUsage(string(body), got.Text)
	}
	return got, nil
}

// kindOf maps a status to a degradation. 429 and 401/403 are the two spec:234
// names, and they are different operational events: one is retriable in a
// minute, the other needs a human.
//
// 400, 404 and 413 are OUR request being wrong — a bad model name, a body the
// provider will not take — not a provider outage. They are malformed_response
// rather than provider_unavailable, because provider_unavailable is the label
// an operator pages on and a bug filed under it is a bug filed as an outage.
func kindOf(status int) Kind {
	switch {
	case status == http.StatusTooManyRequests:
		return KindRateLimited
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return KindUnauthorized
	case status >= 500:
		// 529 overloaded_error included: it is the provider saying it cannot
		// serve this now, which is the same operational fact as a 503.
		return KindUnavailable
	}
	return KindMalformed
}

func wireMessages(ms []Message) []wireMessage {
	out := make([]wireMessage, 0, len(ms))
	for _, m := range ms {
		w := wireMessage{Role: string(m.Role)}
		if m.Text != "" {
			w.Content = append(w.Content, wireBlock{Type: "text", Text: m.Text})
		}
		for _, c := range m.Calls {
			input := c.Args
			if len(input) == 0 {
				input = json.RawMessage(`{}`)
			}
			w.Content = append(w.Content, wireBlock{Type: "tool_use", ID: c.ID, Name: c.Name, Input: input})
		}
		for _, r := range m.Results {
			w.Content = append(w.Content, wireBlock{
				Type: "tool_result", ToolUseID: r.CallID, Content: r.Content, IsError: r.IsError,
			})
		}
		out = append(out, w)
	}
	return out
}

func wireTools(ts []ToolSpec) []wireTool {
	out := make([]wireTool, 0, len(ts))
	for _, t := range ts {
		out = append(out, wireTool{Name: t.Name, Description: t.Description, InputSchema: t.Schema})
	}
	return out
}
