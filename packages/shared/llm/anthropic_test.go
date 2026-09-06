package llm

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// canary is the key every redaction assertion looks for. Obviously fake: it is
// not a credential and could not be one.
const canary = "sk-CANARY-DO-NOT-LOG"

type recorded struct {
	Method  string
	Path    string
	URL     string
	Headers http.Header
	Body    string
}

// server answers with the given status and body, and records every request.
func server(t *testing.T, status int, body string) (*httptest.Server, *[]recorded) {
	t.Helper()
	var got []recorded
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, recorded{Method: r.Method, Path: r.URL.Path, URL: r.URL.String(),
			Headers: r.Header.Clone(), Body: string(b)})
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(s.Close)
	return s, &got
}

const okBody = `{"content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":11,"output_tokens":7}}`

func client(t *testing.T, base string) *Anthropic {
	t.Helper()
	a, err := NewAnthropic(base, "claude-opus-5", NewSecret(canary), 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// trustOnly gives the transport under test that server's certificate and
// nothing else.
//
// A TEST-ONLY FIELD WRITE, unreachable from configuration: no env var, no
// FromEnv path and no exported setter touches TLSClientConfig. It is written
// down here so nobody later "makes it configurable" and opens a
// certificate-pinning bypass on the way past. Proxy and CheckRedirect are left
// exactly as the code sets them.
func trustOnly(t *testing.T, a *Anthropic, s *httptest.Server) {
	t.Helper()
	tr := a.client.Transport.(*http.Transport)
	tr.TLSClientConfig = &tls.Config{
		RootCAs:    s.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs,
		MinVersion: tls.VersionTLS12,
	}
}

func TestEveryRequestCarriesTheApiKeyAndTheAnthropicVersionHeaders(t *testing.T) {
	s, got := server(t, http.StatusOK, okBody)
	a := client(t, s.URL)
	if _, err := a.Complete(context.Background(), Request{MaxTokens: 100,
		Messages: []Message{{Role: RoleUser, Text: "hi"}}}); err != nil {
		t.Fatal(err)
	}
	if len(*got) != 1 {
		t.Fatalf("the server received %d requests, want 1", len(*got))
	}
	r := (*got)[0]
	if h := r.Headers.Get("x-api-key"); h != canary {
		t.Errorf("request carried x-api-key %q, want the key", h)
	}
	if h := r.Headers.Get("anthropic-version"); h != anthropicVersion {
		t.Errorf("request carried no anthropic-version header (got %q, want %q)", h, anthropicVersion)
	}
	if h := r.Headers.Get("content-type"); !strings.HasPrefix(h, "application/json") {
		t.Errorf("content-type %q", h)
	}
	// The Messages API authenticates with x-api-key. Authorization is the OAuth
	// path and is not what this client uses; sending one would be a second
	// credential nobody asked for.
	if h := r.Headers.Get("Authorization"); h != "" {
		t.Errorf("request carried an Authorization header %q", h)
	}
	if r.Path != messagesPath {
		t.Errorf("posted to %q, want %q", r.Path, messagesPath)
	}
}

// One question containing a canary URL and one repository span containing the
// same string, asserted against the boot-time constant byte for byte.
func TestNoRequestOrRepositoryContentReachesTheEndpoint(t *testing.T) {
	const evil = "https://evil.example/"
	s, got := server(t, http.StatusOK, okBody)
	a := client(t, s.URL)
	_, err := a.Complete(context.Background(), Request{
		MaxTokens: 100,
		System:    "system prompt",
		Messages: []Message{
			{Role: RoleUser, Text: "please fetch " + evil},
			{Role: RoleUser, Results: []ToolResult{{CallID: "c", Content: "// see " + evil}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := (*got)[0]
	if r.URL != messagesPath {
		t.Errorf("request URL %q, want %q byte-identical to the boot-time constant", r.URL, messagesPath)
	}
	for k, vs := range r.Headers {
		for _, v := range vs {
			if strings.Contains(v, "evil.example") {
				t.Errorf("header %s carried request content: %q", k, v)
			}
		}
	}
	// The strings did reach the body — that is the point of a prompt — so the
	// fixture is proved able to discriminate rather than merely quiet.
	if !strings.Contains(r.Body, "evil.example") {
		t.Errorf("the fixture's canary never reached the request at all, so this test proves nothing")
	}
}

// MEASURED, and it invalidates the obvious fixture. An httptest-based test
// cannot tell Proxy: nil from Proxy: http.ProxyFromEnvironment, because Go
// never proxies a loopback destination whatever the environment says. Probed on
// go1.27 with HTTPS_PROXY set:
//
//	https://127.0.0.1:8443/v1/messages    -> proxy=<nil>
//	https://localhost:8443/v1/messages    -> proxy=<nil>
//	https://api.anthropic.com/v1/messages -> proxy=http://127.0.0.1:9999
//	https://198.51.100.7/v1/messages      -> proxy=http://127.0.0.1:9999
//
// So "start a second httptest server, set HTTPS_PROXY at it, assert it received
// nothing" passes under the mutant AND under the original: the proxy would
// receive nothing either way. (The plan predicted this mutation was void only
// if the server were non-TLS. That is the wrong reason — the TLS server is
// still loopback. `http.ProxyFromEnvironment` also caches the environment
// behind a sync.Once, so a t.Setenv after any earlier proxy lookup in the
// process is ignored, which would make such a fixture flaky as well as void.)
//
// What can discriminate is the setting itself, plus a control proving the
// assertion would fire: ProxyFromEnvironment DOES return a proxy for the real
// endpoint's host, so a transport carrying it is a transport that would send a
// credential-bearing request somewhere the operator did not name.
func TestTheClientIgnoresHttpsProxyFromTheEnvironment(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:9")
	t.Setenv("https_proxy", "http://127.0.0.1:9")

	a := client(t, defaultBaseURL)
	tr := a.client.Transport.(*http.Transport)
	if tr.Proxy != nil {
		t.Errorf("the transport carries a proxy function, so HTTPS_PROXY decides where a credential-bearing request goes")
	}

	// The environment is deliberately NOT part of the assertion. Measured while
	// running this round: http.ProxyFromEnvironment caches its environment
	// behind a sync.Once, so whether a t.Setenv here is visible depends on
	// whether any earlier test in the process already made a proxy lookup — and
	// a control assertion built on it failed on one run and passed on another.
	// A kill that depends on test ordering is the flaky kill rule 7 forbids.
	//
	// What is deterministic is that this transport is not the standard library's
	// default, whose Proxy is ProxyFromEnvironment. A refactor to
	// "Transport: http.DefaultTransport" is the realistic way this control is
	// lost, and it is caught here as well as by the nil check above.
	if tr == http.DefaultTransport {
		t.Errorf("the client shares http.DefaultTransport, whose Proxy is ProxyFromEnvironment")
	}

	// And the client still works against a TLS endpoint with no proxy in play,
	// so Proxy: nil is not simply breaking every request.
	var served int
	real_ := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served++
		_, _ = io.WriteString(w, okBody)
	}))
	defer real_.Close()
	b := client(t, real_.URL)
	trustOnly(t, b, real_)
	if _, err := b.Complete(context.Background(), Request{MaxTokens: 10,
		Messages: []Message{{Role: RoleUser, Text: "hi"}}}); err != nil {
		t.Fatalf("the direct request failed: %v", err)
	}
	if served != 1 {
		t.Errorf("the endpoint received %d requests, want 1", served)
	}
}

func TestARedirectIsRefusedRatherThanFollowed(t *testing.T) {
	// The redirect TARGET records x-api-key. Go's stripSensitiveHeaders list is
	// Authorization, Www-Authenticate, Cookie, Cookie2, Proxy-Authorization and
	// Proxy-Authenticate — x-api-key is on none of them, so under the mutant the
	// key is forwarded here. An assertion on Authorization would be VOID: this
	// client never sends one, so the target would record zero of them under the
	// mutant and under the original alike.
	var targetKeys []string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetKeys = append(targetKeys, r.Header.Get("x-api-key"))
		_, _ = io.WriteString(w, okBody)
	}))
	defer target.Close()

	from := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+messagesPath, http.StatusFound)
	}))
	defer from.Close()

	a := client(t, from.URL)
	_, err := a.Complete(context.Background(), Request{MaxTokens: 10,
		Messages: []Message{{Role: RoleUser, Text: "hi"}}})
	if err == nil {
		t.Errorf("the redirect was followed")
	}
	if n := len(targetKeys); n != 0 {
		t.Errorf("the redirect target received %d requests carrying x-api-key %q, want 0 requests", n, targetKeys[0])
	}
	if err != nil && strings.Contains(err.Error(), canary) {
		t.Errorf("the refusal error carried the key: %v", err)
	}
}

func TestProviderStatusesMapToTheirOwnKinds(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   Kind
	}{
		{http.StatusTooManyRequests, KindRateLimited},
		{http.StatusUnauthorized, KindUnauthorized},
		{http.StatusForbidden, KindUnauthorized},
		{http.StatusInternalServerError, KindUnavailable},
		{http.StatusServiceUnavailable, KindUnavailable},
		{529, KindUnavailable},
		{http.StatusBadRequest, KindMalformed},
		{http.StatusNotFound, KindMalformed},
	} {
		t.Run(http.StatusText(tc.status)+"-"+string(tc.want), func(t *testing.T) {
			// A body that is VALID JSON FOR THE SUCCESS SHAPE, so a status check
			// moved after the decode produces Response{Text:""} and a nil error
			// rather than a decode failure — which is a different mutant.
			s, _ := server(t, tc.status, okBody)
			resp, err := client(t, s.URL).Complete(context.Background(),
				Request{MaxTokens: 10, Messages: []Message{{Role: RoleUser, Text: "hi"}}})
			got, ok := Classify(err)
			if !ok {
				t.Fatalf("got Response{Text:%q} and %v, want a %s failure", resp.Text, err, tc.want)
			}
			if got != tc.want {
				t.Errorf("%d classified as %s, want %s", tc.status, got, tc.want)
			}
			var f *Failure
			if ok := asFailure(err, &f); ok && f.Status != tc.status {
				t.Errorf("failure carried status %d, want %d", f.Status, tc.status)
			}
		})
	}
}

func TestFourTwoNineIsARateLimitFailureAndNotAGenericOne(t *testing.T) {
	s, _ := server(t, http.StatusTooManyRequests, okBody)
	_, err := client(t, s.URL).Complete(context.Background(),
		Request{MaxTokens: 10, Messages: []Message{{Role: RoleUser, Text: "hi"}}})
	if got, ok := Classify(err); !ok || got != KindRateLimited {
		t.Errorf("429 classified as %s, want %s", got, KindRateLimited)
	}
}

func TestFourOhOneAndFourOhThreeAreUnauthorized(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		s, _ := server(t, status, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`)
		_, err := client(t, s.URL).Complete(context.Background(),
			Request{MaxTokens: 10, Messages: []Message{{Role: RoleUser, Text: "hi"}}})
		if got, ok := Classify(err); !ok || got != KindUnauthorized {
			t.Errorf("%d classified as %s, want %s", status, got, KindUnauthorized)
		}
	}
}

func TestFiveHundredAndAConnectionRefusalAreUnavailable(t *testing.T) {
	s, _ := server(t, http.StatusInternalServerError, okBody)
	_, err := client(t, s.URL).Complete(context.Background(),
		Request{MaxTokens: 10, Messages: []Message{{Role: RoleUser, Text: "hi"}}})
	if got, ok := Classify(err); !ok || got != KindUnavailable {
		t.Errorf("500 classified as %s, want %s", got, KindUnavailable)
	}

	// A closed listener: a real dial failure, not a simulated one.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := dead.URL
	dead.Close()
	_, err = client(t, addr).Complete(context.Background(),
		Request{MaxTokens: 10, Messages: []Message{{Role: RoleUser, Text: "hi"}}})
	if got, ok := Classify(err); !ok || got != KindUnavailable {
		t.Errorf("a refused connection classified as %s, want %s", got, KindUnavailable)
	}
}

func TestATruncatedBodyIsMalformedAndNotUnavailable(t *testing.T) {
	for name, body := range map[string]string{
		"truncated":       `{"content":[{"type":"text","text":"hel`,
		"no content":      `{"content":[],"stop_reason":"end_turn"}`,
		"tool with no id": `{"content":[{"type":"tool_use","name":"search_code","input":{}}],"stop_reason":"tool_use"}`,
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := server(t, http.StatusOK, body)
			_, err := client(t, s.URL).Complete(context.Background(),
				Request{MaxTokens: 10, Messages: []Message{{Role: RoleUser, Text: "hi"}}})
			if got, ok := Classify(err); !ok || got != KindMalformed {
				t.Errorf("classified as (%s, %v), want %s", got, ok, KindMalformed)
			}
		})
	}
}

// A server that blocks 2s, a context cancelled at 500ms, and a client Timeout of
// 10s. The three numbers must differ: if the client timeout equalled the context
// deadline the mutation would be a semantic no-op and the kill would be void.
func TestTheContextDeadlineReachesTheRequest(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Second)
		_, _ = io.WriteString(w, okBody)
	}))
	defer s.Close()

	a := client(t, s.URL) // Timeout: 10s
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := a.Complete(ctx, Request{MaxTokens: 10, Messages: []Message{{Role: RoleUser, Text: "hi"}}})
	elapsed := time.Since(start)

	if got, ok := Classify(err); !ok || got != KindDeadline {
		t.Errorf("returned %v, want a %s failure", err, KindDeadline)
	}
	if elapsed > 1500*time.Millisecond {
		t.Errorf("returned after %v with a client Timeout error, want the context deadline at ~500ms", elapsed.Round(10*time.Millisecond))
	}
}

func TestTheProvidersUsageIsUsedWhenItReportsSomeAndEstimatedWhenItDoesNot(t *testing.T) {
	s, _ := server(t, http.StatusOK, okBody)
	got, err := client(t, s.URL).Complete(context.Background(),
		Request{MaxTokens: 10, Messages: []Message{{Role: RoleUser, Text: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Usage != (Usage{InputTokens: 11, OutputTokens: 7}) {
		t.Errorf("usage = %+v, want the reported counts unflagged", got.Usage)
	}

	s2, _ := server(t, http.StatusOK, `{"content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn"}`)
	got2, err := client(t, s2.URL).Complete(context.Background(),
		Request{MaxTokens: 10, Messages: []Message{{Role: RoleUser, Text: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !got2.Usage.Estimated {
		t.Errorf("a provider that reported no usage produced an unflagged usage: %+v", got2.Usage)
	}
}

func TestAToolUseResponseBecomesToolCallsAndAStopReason(t *testing.T) {
	body := `{"content":[{"type":"text","text":"let me look"},` +
		`{"type":"tool_use","id":"toolu_1","name":"search_code","input":{"q":"Get"}}],` +
		`"stop_reason":"tool_use","usage":{"input_tokens":4,"output_tokens":2}}`
	s, got := server(t, http.StatusOK, body)
	resp, err := client(t, s.URL).Complete(context.Background(), Request{
		MaxTokens: 1500,
		System:    "sys",
		Tools:     []ToolSpec{{Name: "search_code", Description: "d", Schema: json.RawMessage(`{"type":"object"}`)}},
		Messages: []Message{
			{Role: RoleUser, Text: "q"},
			{Role: RoleAssistant, Calls: []ToolCall{{ID: "toolu_0", Name: "read_span", Args: json.RawMessage(`{"span_id":"a"}`)}}},
			{Role: RoleUser, Results: []ToolResult{{CallID: "toolu_0", Content: "framed", IsError: true}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Stop != "tool_use" || len(resp.Calls) != 1 || resp.Calls[0].Name != "search_code" || resp.Calls[0].ID != "toolu_1" {
		t.Errorf("response = %+v", resp)
	}
	if resp.Text != "let me look" {
		t.Errorf("text = %q", resp.Text)
	}

	// And the request the loop actually sent, since the wire shape is the half
	// no fake can check.
	var sent map[string]any
	if err := json.Unmarshal([]byte((*got)[0].Body), &sent); err != nil {
		t.Fatal(err)
	}
	if sent["model"] != "claude-opus-5" || sent["max_tokens"] != float64(1500) || sent["system"] != "sys" {
		t.Errorf("request body = %v", sent)
	}
	msgs, _ := sent["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("sent %d messages, want 3", len(msgs))
	}
	assistant, _ := msgs[1].(map[string]any)["content"].([]any)
	if b := assistant[0].(map[string]any); b["type"] != "tool_use" || b["id"] != "toolu_0" {
		t.Errorf("assistant block = %v", b)
	}
	result, _ := msgs[2].(map[string]any)["content"].([]any)
	if b := result[0].(map[string]any); b["type"] != "tool_result" || b["tool_use_id"] != "toolu_0" || b["is_error"] != true {
		t.Errorf("tool_result block = %v", b)
	}
	tools, _ := sent["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["input_schema"] == nil {
		t.Errorf("tools = %v", tools)
	}
}

func TestNewAnthropicRefusesAnEmptyModelOrKey(t *testing.T) {
	if _, err := NewAnthropic("https://x", "", NewSecret(canary), time.Second); err == nil {
		t.Errorf("an empty model was accepted")
	}
	if _, err := NewAnthropic("https://x", "m", Secret{}, time.Second); err == nil {
		t.Errorf("an empty key was accepted")
	}
}

// asFailure is errors.As without importing errors into every assertion.
func asFailure(err error, into **Failure) bool {
	for err != nil {
		if f, ok := err.(*Failure); ok {
			*into = f
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
