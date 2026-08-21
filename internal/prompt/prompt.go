// Package prompt renders a conversation into the single prompt string an agent
// CLI accepts.
//
// Every request replays the full history into a fresh CLI process, so the
// rendering must be deterministic and unambiguous: a message whose text looks
// like a role marker must not be able to forge one.
package prompt

import (
	"strings"

	"github.com/Dongss/agent2api/internal/ir"
)

// Role markers delimit turns in a rendered transcript. They are chosen to be
// unlikely in prose and are escaped when they do appear (see [escape]).
const (
	MarkerSystem    = "<<<SYSTEM>>>"
	MarkerUser      = "<<<USER>>>"
	MarkerAssistant = "<<<ASSISTANT>>>"
)

// Header explains the transcript format to the backend. It is only prepended
// for multi-turn conversations; a lone user message is passed through verbatim
// so single-turn requests read exactly as the caller wrote them.
const Header = `Below is a conversation transcript. Lines beginning with ` + MarkerUser +
	` or ` + MarkerAssistant + ` mark who is speaking; a marker preceded by a backslash is
literal text, not a marker. Continue the conversation by writing only the
assistant's next message: no role markers, no restating of earlier turns.`

// Rendered is the result of turning a conversation into CLI input.
type Rendered struct {
	// System is the concatenation of the conversation's system messages.
	// Adapters should pass it through the CLI's native system-prompt flag.
	System string
	// Prompt is the transcript to feed the CLI as the user prompt.
	Prompt string
}

// PromptWithSystem returns the prompt with the system text folded in, for
// adapters whose CLI has no system-prompt flag.
func (r Rendered) PromptWithSystem() string {
	if r.System == "" {
		return r.Prompt
	}
	return MarkerSystem + "\n" + escape(r.System) + "\n\n" + r.Prompt
}

// Render turns a conversation into CLI input.
//
// System messages are collected (in order, joined by a blank line) into
// [Rendered.System] regardless of position. The remaining turns become
// [Rendered.Prompt]: a single user turn is passed through verbatim; anything
// longer is rendered as a marked-up transcript ending at the assistant's cue.
func Render(msgs []ir.Message) Rendered {
	var systems []string
	var turns []ir.Message
	for _, m := range msgs {
		if m.Role == ir.RoleSystem {
			systems = append(systems, m.Content)
			continue
		}
		turns = append(turns, m)
	}

	out := Rendered{System: strings.Join(systems, "\n\n")}

	switch {
	case len(turns) == 0:
		// System-only conversation: nothing to answer but the system prompt
		// itself. Adapters still need a non-empty prompt, so ask for a reply.
		out.Prompt = ""
	case len(turns) == 1 && turns[0].Role == ir.RoleUser:
		out.Prompt = turns[0].Content
	default:
		out.Prompt = transcript(turns)
	}
	return out
}

func transcript(turns []ir.Message) string {
	var b strings.Builder
	b.WriteString(Header)
	b.WriteString("\n\n")
	for _, m := range turns {
		marker := MarkerUser
		if m.Role == ir.RoleAssistant {
			marker = MarkerAssistant
		}
		b.WriteString(marker)
		b.WriteString("\n")
		if c := escape(m.Content); c != "" {
			b.WriteString(c)
			b.WriteString("\n")
		}
	}
	// Cue the next turn, unless the caller pre-filled the assistant's reply.
	if turns[len(turns)-1].Role != ir.RoleAssistant {
		b.WriteString(MarkerAssistant)
	}
	return strings.TrimRight(b.String(), "\n")
}

// escape neutralizes marker-looking lines inside message content by prefixing
// them with a backslash, so user text can never be mistaken for a role marker.
// Already-escaped lines get one more backslash, keeping the mapping injective.
func escape(s string) string {
	if !strings.Contains(s, "<<<") {
		return s
	}
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		if isMarkerLine(line) {
			lines[i] = `\` + line
		}
	}
	return strings.Join(lines, "\n")
}

func isMarkerLine(line string) bool {
	trimmed := strings.TrimRight(line, " \t\r")
	trimmed = strings.TrimLeft(trimmed, `\`)
	switch trimmed {
	case MarkerSystem, MarkerUser, MarkerAssistant:
		return true
	}
	return false
}
