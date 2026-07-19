package main

import (
	"testing"
)

func TestParseRecognitionText(t *testing.T) {
	message := `{"header":{"name":"RecognitionCompleted"},"payload":{"result":"打开项目并运行测试"}}`
	if got := parseRecognitionText(message); got != "打开项目并运行测试" {
		t.Fatalf("result = %q", got)
	}
	if got := parseRecognitionText(`{"payload":{}}`); got != "" {
		t.Fatalf("empty result = %q", got)
	}
}
