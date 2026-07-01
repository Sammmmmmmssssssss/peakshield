package stripper

import (
	"bytes"
	"strings"
	"testing"
)

func TestStripHTML(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected []string
		omitted  []string
	}{
		{
			name:     "Strip Script",
			input:    `<html><head><script>alert(1);</script></head><body>Hello</body></html>`,
			expected: []string{"<html><head></head><body>", "Hello</body></html>"},
			omitted:  []string{"<script>"},
		},
		{
			name:     "Strip Link Stylesheet",
			input:    `<html><head><link rel="stylesheet" href="style.css"></head><body>Hi</body></html>`,
			expected: []string{"<html><head></head><body>", "Hi</body></html>"},
			omitted:  []string{"<link "},
		},
		{
			name:     "Inject Notice",
			input:    `<html><body><p>Test</p></body></html>`,
			expected: []string{"<body>", "PeakShield Active", "<p>Test</p>"},
			omitted:  []string{},
		},
		{
			name:     "Replace Image",
			input:    `<body><img src="huge.png"></body>`,
			expected: []string{"<span>[img]</span>"},
			omitted:  []string{"<img"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := bytes.NewBuffer(nil)
			stripHTML([]byte(tt.input), out)
			result := out.String()

			for _, exp := range tt.expected {
				if !strings.Contains(result, exp) {
					t.Errorf("Expected to contain %q, got: %q", exp, result)
				}
			}
			for _, omit := range tt.omitted {
				if strings.Contains(result, omit) {
					t.Errorf("Expected to omit %q, got: %q", omit, result)
				}
			}
		})
	}
}

// FuzzStripper tests the hand-rolled HTML tokenizer against arbitrary byte
// sequences to ensure it never panics, hangs, or reads out of bounds, no matter
// how malformed the input is.
func FuzzStripper(f *testing.F) {
	// Add seeds of valid HTML
	f.Add([]byte(`<html><head><script>alert(1);</script></head><body>Hello</body></html>`))
	f.Add([]byte(`<html><head><link rel="stylesheet" href="style.css"></head><body>Hi</body></html>`))
	// Add seeds of heavily malformed tags
	f.Add([]byte(`<script`))
	f.Add([]byte(`< img src=`))
	f.Add([]byte(`<!-- unclosed comment`))
	f.Add([]byte(`<script>  < / script >`))
	f.Add([]byte(`<!DOCTYPE html><html><body><h1>Title</h1></body></html>`))

	f.Fuzz(func(t *testing.T, input []byte) {
		out := bytes.NewBuffer(nil)
		// We don't care about the output contents here, only that stripHTML
		// does not panic, hang (infinite loop), or crash.
		// If it panics, the fuzzer will catch it and fail the test.
		stripHTML(input, out)
	})
}
