package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"reflect"
	"testing"
)

func TestParseModelReplacements(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected map[string]string
	}{
		{
			name:     "Empty input",
			input:    "",
			expected: map[string]string{},
		},
		{
			name:     "JSON format",
			input:    `{"GEMINI": "gemini-flash", "chatgpt": "gpt-4o"}`,
			expected: map[string]string{"GEMINI": "gemini-flash", "chatgpt": "gpt-4o"},
		},
		{
			name:     "User's custom format with quotes",
			input:    `"GEMINI:gemini-flash", "chatgpt:gpt-4o"`,
			expected: map[string]string{"GEMINI": "gemini-flash", "chatgpt": "gpt-4o"},
		},
		{
			name:     "Simple comma-separated format",
			input:    "GEMINI:gemini-flash,chatgpt:gpt-4o",
			expected: map[string]string{"GEMINI": "gemini-flash", "chatgpt": "gpt-4o"},
		},
		{
			name:     "Whitespace and mixed quotes",
			input:    ` 'GEMINI' : 'gemini-flash' , "chatgpt" : "gpt-4o" `,
			expected: map[string]string{"GEMINI": "gemini-flash", "chatgpt": "gpt-4o"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseModelReplacements(tt.input)
			if !reflect.DeepEqual(got, tt.expected) {
				t.Errorf("parseModelReplacements(%q) = %v; want %v", tt.input, got, tt.expected)
			}
		})
	}
}

func TestReplaceModelInPath(t *testing.T) {
	replacements := map[string]string{
		"GEMINI":  "gemini-flash",
		"chatgpt": "gpt-4o",
	}

	tests := []struct {
		name     string
		path     string
		expected string
	}{
		{
			name:     "No model in path",
			path:     "/v1/chat/completions",
			expected: "/v1/chat/completions",
		},
		{
			name:     "Gemini style path with colon",
			path:     "/v1beta/models/GEMINI:generateContent",
			expected: "/v1beta/models/gemini-flash:generateContent",
		},
		{
			name:     "Model as simple path segment",
			path:     "/v1/models/GEMINI",
			expected: "/v1/models/gemini-flash",
		},
		{
			name:     "Model in path with trailing slash",
			path:     "/v1/models/GEMINI/",
			expected: "/v1/models/gemini-flash/",
		},
		{
			name:     "Multiple segments with matching text but not exact segment match",
			path:     "/v1/models/GEMINI_PRO",
			expected: "/v1/models/GEMINI_PRO", // shouldn't match GEMINI
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := replaceModelInPath(tt.path, replacements)
			if got != tt.expected {
				t.Errorf("replaceModelInPath(%q) = %q; want %q", tt.path, got, tt.expected)
			}
		})
	}
}

func TestReplaceModelInBody(t *testing.T) {
	replacements := map[string]string{
		"GEMINI":  "gemini-flash",
		"chatgpt": "gpt-4o",
	}

	tests := []struct {
		name        string
		body        string
		expected    string
		shouldAlter bool
	}{
		{
			name:        "Non-JSON body",
			body:        "plain text payload",
			expected:    "plain text payload",
			shouldAlter: false,
		},
		{
			name:        "JSON without model key",
			body:        `{"messages":[{"role":"user","content":"hello"}]}`,
			expected:    `{"messages":[{"role":"user","content":"hello"}]}`,
			shouldAlter: false,
		},
		{
			name:        "JSON with non-matching model",
			body:        `{"model":"claude-3","messages":[]}`,
			expected:    `{"model":"claude-3","messages":[]}`,
			shouldAlter: false,
		},
		{
			name:        "JSON with matching model",
			body:        `{"model":"GEMINI","messages":[],"temperature":0.7}`,
			expected:    `{"model":"gemini-flash","messages":[],"temperature":0.7}`,
			shouldAlter: true,
		},
		{
			name:        "JSON preserving number types",
			body:        `{"model":"chatgpt","temperature":0.7,"max_tokens":150}`,
			expected:    `{"model":"gpt-4o","temperature":0.7,"max_tokens":150}`,
			shouldAlter: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest("POST", "/test", bytes.NewBufferString(tt.body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")

			replaceModelInBody(req, replacements)

			gotBytes, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatal(err)
			}
			got := string(gotBytes)

			if tt.shouldAlter {
				// Compare JSON equivalence
				var gotMap, expectedMap map[string]interface{}
				if err := json.Unmarshal(gotBytes, &gotMap); err != nil {
					t.Fatalf("failed to parse got JSON %q: %v", got, err)
				}
				if err := json.Unmarshal([]byte(tt.expected), &expectedMap); err != nil {
					t.Fatalf("failed to parse expected JSON %q: %v", tt.expected, err)
				}
				if !reflect.DeepEqual(gotMap, expectedMap) {
					t.Errorf("replaceModelInBody JSON mismatch. got: %s, want: %s", got, tt.expected)
				}
			} else {
				if got != tt.expected {
					t.Errorf("replaceModelInBody mismatch. got: %s, want: %s", got, tt.expected)
				}
			}
		})
	}
}

func TestProxyIntegration(t *testing.T) {
	var receivedPath string
	var receivedBody []byte

	targetServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		receivedPath = req.URL.Path
		var err error
		receivedBody, err = io.ReadAll(req.Body)
		if err != nil {
			t.Errorf("failed to read target request body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	defer targetServer.Close()

	targetURL, err := url.Parse(targetServer.URL)
	if err != nil {
		t.Fatal(err)
	}

	proxy := httputil.NewSingleHostReverseProxy(targetURL)
	originalDirector := proxy.Director

	replacements := map[string]string{
		"GEMINI": "gemini-flash",
	}

	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		req.Host = targetURL.Host

		req.URL.Path = replaceModelInPath(req.URL.Path, replacements)
		replaceModelInBody(req, replacements)
	}

	reqBody := `{"model":"GEMINI","prompt":"hello"}`
	req := httptest.NewRequest("POST", "/v1/models/GEMINI:generateContent", bytes.NewBufferString(reqBody))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 OK, got %d", rec.Code)
	}

	expectedPath := "/v1/models/gemini-flash:generateContent"
	if receivedPath != expectedPath {
		t.Errorf("forwarded path = %q; want %q", receivedPath, expectedPath)
	}

	var parsedBody map[string]interface{}
	if err := json.Unmarshal(receivedBody, &parsedBody); err != nil {
		t.Fatalf("failed to parse forwarded body: %v", err)
	}

	if parsedBody["model"] != "gemini-flash" {
		t.Errorf("forwarded model = %v; want %q", parsedBody["model"], "gemini-flash")
	}
}
