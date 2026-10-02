package llm

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"
)

// debugMax caps how much of a model answer --debug prints.
const debugMax = 4096

// debugf writes one --debug line when w is set. Callers never pass credentials.
func debugf(w io.Writer, format string, args ...any) {
	if w != nil {
		fmt.Fprintf(w, format+"\n", args...)
	}
}

// describeRequest summarizes a request by size: never its prompt text or images.
func describeRequest(req Request) string {
	images, imgBytes, text := 0, 0, 0
	for _, p := range req.Parts {
		if p.JPEG != nil {
			images++
			imgBytes += len(p.JPEG)
		} else {
			text += len(p.Text)
		}
	}
	s := fmt.Sprintf("%s: %d image(s) %d KB, %d chars of text, system prompt %d chars", req.SchemaName, images, imgBytes/1024, text, len(req.System))
	if req.Effort != "" {
		s += ", effort " + req.Effort
	}
	if req.MaxTokens > 0 {
		s += fmt.Sprintf(", max_tokens %d", req.MaxTokens)
	}
	return s
}

// logged wraps one attempt of a call so --debug shows its time, usage and answer.
func logged(w io.Writer, name string, fn func(context.Context) (*Response, error)) func(context.Context) (*Response, error) {
	if w == nil {
		return fn
	}
	return func(ctx context.Context) (*Response, error) {
		start := time.Now()
		resp, err := fn(ctx)
		took := time.Since(start).Round(time.Millisecond)
		switch {
		case resp != nil:
			debugf(w, "%s: answered in %s; tokens in=%d (cache written %d, read %d) out=%d; %s",
				name, took, resp.Usage.TotalIn(), resp.Usage.CacheWriteTokens, resp.Usage.CacheReadTokens, resp.Usage.OutputTokens,
				strings.TrimSpace(truncate(resp.JSON, debugMax)))
			if err != nil {
				debugf(w, "%s: error: %v", name, err)
			}
		case err != nil:
			debugf(w, "%s: failed after %s: %v", name, took, err)
		}
		return resp, err
	}
}

// redactArgs replaces the prompt and schema arguments of a claude invocation by
// their lengths: they are long, and the prompt is ours, not news.
func redactArgs(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		switch {
		case i > 0 && (args[i-1] == "--system-prompt" || args[i-1] == "--json-schema"):
			out[i] = fmt.Sprintf("<%d chars>", len(a))
		case a == "":
			out[i] = `""`
		default:
			out[i] = a
		}
	}
	return strings.Join(out, " ")
}
