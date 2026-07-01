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

func TestMaskKey(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"Empty key", "", "***"},
		{"Short key", "12345", "***"},
		{"8-char key", "12345678", "***"},
		{"9-char key", "123456789", "123...6789"},
		{"Long key", "sk-proj-abcdef123456", "sk-...3456"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := maskKey(tt.input)
			if got != tt.expected {
				t.Errorf("maskKey(%q) = %q; want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestReplaceAPIKeys(t *testing.T) {
	replacements := map[string]string{
		"client-key-1": "upstream-key-1",
		"client-key-2": "upstream-key-2",
	}

	tests := []struct {
		name           string
		method         string
		url            string
		headers        map[string]string
		expectedHeader map[string]string
		expectedURL    string
	}{
		{
			name:   "Auth header Bearer replacement",
			method: "POST",
			url:    "/v1/chat",
			headers: map[string]string{
				"Authorization": "Bearer client-key-1",
			},
			expectedHeader: map[string]string{
				"Authorization": "Bearer upstream-key-1",
			},
			expectedURL: "/v1/chat",
		},
		{
			name:   "Auth header raw replacement",
			method: "POST",
			url:    "/v1/chat",
			headers: map[string]string{
				"Authorization": "client-key-2",
			},
			expectedHeader: map[string]string{
				"Authorization": "upstream-key-2",
			},
			expectedURL: "/v1/chat",
		},
		{
			name:   "api-key and x-api-key headers",
			method: "GET",
			url:    "/v1/models",
			headers: map[string]string{
				"api-key":   "client-key-1",
				"x-api-key": "client-key-2",
			},
			expectedHeader: map[string]string{
				"api-key":   "upstream-key-1",
				"x-api-key": "upstream-key-2",
			},
			expectedURL: "/v1/models",
		},
		{
			name:           "Query param replacement (key)",
			method:         "GET",
			url:            "/v1beta/models/gemini-pro:generateContent?key=client-key-1",
			headers:        map[string]string{},
			expectedHeader: map[string]string{},
			expectedURL:    "/v1beta/models/gemini-pro:generateContent?key=upstream-key-1",
		},
		{
			name:           "Query param replacement (api_key and api-key)",
			method:         "GET",
			url:            "/v1beta/models/gemini-pro:generateContent?api_key=client-key-1&api-key=client-key-2&other=keep",
			headers:        map[string]string{},
			expectedHeader: map[string]string{},
			expectedURL:    "/v1beta/models/gemini-pro:generateContent?api-key=upstream-key-2&api_key=upstream-key-1&other=keep",
		},
		{
			name:   "Unrelated key in header and query remains unchanged",
			method: "POST",
			url:    "/v1/chat?key=unrelated-key",
			headers: map[string]string{
				"Authorization": "Bearer unrelated-key",
			},
			expectedHeader: map[string]string{
				"Authorization": "Bearer unrelated-key",
			},
			expectedURL: "/v1/chat?key=unrelated-key",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest(tt.method, tt.url, nil)
			if err != nil {
				t.Fatal(err)
			}
			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}

			replaceAPIKeys(req, replacements)

			// Check headers
			for k, expectedVal := range tt.expectedHeader {
				if gotVal := req.Header.Get(k); gotVal != expectedVal {
					t.Errorf("expected header %s to be %q, got %q", k, expectedVal, gotVal)
				}
			}

			// Check URL
			expectedParsed, err := url.Parse(tt.expectedURL)
			if err != nil {
				t.Fatal(err)
			}
			gotParsed := req.URL

			if gotParsed.Path != expectedParsed.Path {
				t.Errorf("expected URL path %q, got %q", expectedParsed.Path, gotParsed.Path)
			}

			// Compare query parameters map to avoid ordering issues with query.Encode()
			expectedQuery := expectedParsed.Query()
			gotQuery := gotParsed.Query()
			if !reflect.DeepEqual(gotQuery, expectedQuery) {
				t.Errorf("expected URL query %+v, got %+v", expectedQuery, gotQuery)
			}
		})
	}
}

func TestProxyAPIKeyIntegration(t *testing.T) {
	var receivedAuthHeader string
	var receivedApiKeyHeader string
	var receivedXApiKeyHeader string
	var receivedQueryKey string

	targetServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		receivedAuthHeader = req.Header.Get("Authorization")
		receivedApiKeyHeader = req.Header.Get("api-key")
		receivedXApiKeyHeader = req.Header.Get("x-api-key")
		receivedQueryKey = req.URL.Query().Get("key")
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
		"client-key": "upstream-key",
	}

	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		req.Host = targetURL.Host
		replaceAPIKeys(req, replacements)
	}

	req := httptest.NewRequest("POST", "/v1/chat?key=client-key", nil)
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("api-key", "client-key")
	req.Header.Set("x-api-key", "client-key")

	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 OK, got %d", rec.Code)
	}

	if receivedAuthHeader != "Bearer upstream-key" {
		t.Errorf("forwarded Authorization = %q; want %q", receivedAuthHeader, "Bearer upstream-key")
	}
	if receivedApiKeyHeader != "upstream-key" {
		t.Errorf("forwarded api-key = %q; want %q", receivedApiKeyHeader, "upstream-key")
	}
	if receivedXApiKeyHeader != "upstream-key" {
		t.Errorf("forwarded x-api-key = %q; want %q", receivedXApiKeyHeader, "upstream-key")
	}
	if receivedQueryKey != "upstream-key" {
		t.Errorf("forwarded query key = %q; want %q", receivedQueryKey, "upstream-key")
	}
}

func TestWildcardReplacements(t *testing.T) {
	t.Run("Model Path Wildcard", func(t *testing.T) {
		replacements := map[string]string{
			"GEMINI": "gemini-flash",
			"*":      "fallback-model",
		}
		
		// Exact match still works
		got := replaceModelInPath("/v1/models/GEMINI", replacements)
		if got != "/v1/models/gemini-flash" {
			t.Errorf("expected /v1/models/gemini-flash, got %q", got)
		}
		
		// Unmatched model after /models/ segment gets replaced by wildcard
		got = replaceModelInPath("/v1/models/claude-3", replacements)
		if got != "/v1/models/fallback-model" {
			t.Errorf("expected /v1/models/fallback-model, got %q", got)
		}
		
		// Other path segments (not preceded by "models") must NOT be replaced
		got = replaceModelInPath("/v1/chat/completions", replacements)
		if got != "/v1/chat/completions" {
			t.Errorf("expected /v1/chat/completions to remain unchanged, got %q", got)
		}
	})

	t.Run("Model Body Wildcard", func(t *testing.T) {
		replacements := map[string]string{
			"GEMINI":  "gemini-flash",
			"default": "default-model",
		}
		
		req, err := http.NewRequest("POST", "/chat", bytes.NewBufferString(`{"model":"claude-3"}`))
		if err != nil {
			t.Fatal(err)
		}
		replaceModelInBody(req, replacements)
		
		var body map[string]interface{}
		gotBytes, _ := io.ReadAll(req.Body)
		json.Unmarshal(gotBytes, &body)
		if body["model"] != "default-model" {
			t.Errorf("expected default-model, got %v", body["model"])
		}
	})

	t.Run("API Key Wildcard", func(t *testing.T) {
		replacements := map[string]string{
			"known-client": "known-upstream",
			"*":            "default-upstream",
		}
		
		req, err := http.NewRequest("GET", "/test?key=unknown-client", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer another-unknown-client")
		req.Header.Set("api-key", "known-client") // exact match
		
		replaceAPIKeys(req, replacements)
		
		if req.Header.Get("api-key") != "known-upstream" {
			t.Errorf("expected known-upstream for api-key, got %q", req.Header.Get("api-key"))
		}
		if req.Header.Get("Authorization") != "Bearer default-upstream" {
			t.Errorf("expected Bearer default-upstream, got %q", req.Header.Get("Authorization"))
		}
		if req.URL.Query().Get("key") != "default-upstream" {
			t.Errorf("expected default-upstream in query, got %q", req.URL.Query().Get("key"))
		}
	})
}
