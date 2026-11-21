package app

import (
	"strings"
	"testing"
)

func TestContextFilePromptFormatting(t *testing.T) {
	// 1. latest is empty
	pEmpty := contextFilePrompt("", 20000)
	if !strings.Contains(pEmpty, "The latest user request is at the end of `message.txt`.") {
		t.Errorf("expected mention of latest request at end of file, got: %s", pEmpty)
	}

	// 2. latest is short (<= 4000)
	shortMsg := "What is the return code?"
	pShort := contextFilePrompt(shortMsg, 20000)
	if !strings.Contains(pShort, "Latest user request:\nWhat is the return code?") {
		t.Errorf("expected inlined latest user request, got: %s", pShort)
	}

	// 3. latest is very long (> 4000)
	longMsg := strings.Repeat("A very long message. ", 300)
	pLong := contextFilePrompt(longMsg, 20000)
	if !strings.Contains(pLong, "The latest user request is at the end of `message.txt`; read it from there") {
		t.Errorf("expected directive to read long user request from file, got: %s", pLong)
	}
	if strings.Contains(pLong, longMsg) {
		t.Errorf("long latest request should not be inlined")
	}
}

func TestPrepareContextFile_Limits(t *testing.T) {
	// Under budget -> used = false
	prompt := "Small prompt under 20000 bytes"
	p, files, used, err := prepareContextFile(prompt, prompt, 20000, "dummy-cookie", 0, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if used || files != nil || p != prompt {
		t.Errorf("expected unused when under budget")
	}

	// Anonymous -> used = false
	largePrompt := strings.Repeat("Large prompt text. ", 1500) // ~30KB
	p, files, used, err = prepareContextFile(largePrompt, "latest", 20000, "", 0, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if used || files != nil {
		t.Errorf("expected unused when cookie is empty")
	}

	// Exceeds MaxContextFileBytes (160,000) -> returns PromptTooLongError
	massivePrompt := strings.Repeat("Massive content. ", 12000) // ~200KB
	_, _, _, err = prepareContextFile(massivePrompt, "latest", 20000, "dummy-cookie", 0, "")
	if err == nil {
		t.Fatalf("expected error when prompt exceeds MaxContextFileBytes")
	}
	ptl, ok := err.(*PromptTooLongError)
	if !ok {
		t.Fatalf("expected *PromptTooLongError, got %T: %v", err, err)
	}
	if ptl.Budget != MaxContextFileBytes {
		t.Errorf("expected budget %d, got %d", MaxContextFileBytes, ptl.Budget)
	}
}
