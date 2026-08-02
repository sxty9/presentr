package api

import (
	"strings"
	"testing"
)

// selectRelevantText keeps the paragraphs most relevant to the question, within the byte budget, in
// reading order — the "use the passende Abschnitte, not everything" behaviour that lets a big read still
// ground an answer.
func TestSelectRelevantTextPicksRelevantParagraphs(t *testing.T) {
	// A large read with three sections; only the middle one is about the projector.
	text := strings.Join([]string{
		"Section about the wall plate and its HDMI terminals near the door.",
		"The projector is an Epson EB-2250U mounted on the ceiling; its lamp life is rated at 5000 hours.",
		"General safety notes about cabling and cable management under the floor.",
	}, "\n\n")

	// A budget big enough for one paragraph forces a choice; the projector paragraph must win.
	kept := selectRelevantText(text, "How long does the projector lamp last?", 120)
	if kept == "" {
		t.Fatal("selection returned nothing")
	}
	if !strings.Contains(kept, "Epson EB-2250U") {
		t.Fatalf("the projector paragraph should be selected, got: %q", kept)
	}
	if strings.Contains(kept, "wall plate") {
		t.Fatalf("an unrelated paragraph should not have been chosen at this budget: %q", kept)
	}
}

// With no question, the leading text is kept (a stable, non-empty grounding rather than nothing).
func TestSelectRelevantTextNoQuestionKeepsLeadingText(t *testing.T) {
	text := "First paragraph of the manual.\n\nSecond paragraph.\n\nThird paragraph."
	kept := selectRelevantText(text, "", 40)
	if !strings.Contains(kept, "First paragraph") {
		t.Fatalf("with no question the leading paragraph should be kept, got: %q", kept)
	}
}

// The selection never exceeds the budget.
func TestSelectRelevantTextRespectsBudget(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 50; i++ {
		b.WriteString("Paragraph number with the word projector in it, discussing HDMI wiring.\n\n")
	}
	kept := selectRelevantText(b.String(), "projector HDMI", 300)
	if len(kept) > 300 {
		t.Fatalf("selection is %d bytes, over the 300-byte budget", len(kept))
	}
	if kept == "" {
		t.Fatal("selection should include at least one paragraph")
	}
}
