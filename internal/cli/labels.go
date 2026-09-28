package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/jefflaplante/gophotocull/internal/labels"
)

// userLabels loads your labels for a report: path when given (it must exist), else
// the log beside the report when there is one; none with --no-labels. Sidecars and
// moves then follow your verdicts, so they never undo what you decided in review.
func userLabels(w io.Writer, reportPath, path string, off bool) (map[string]labels.Entry, error) {
	if off {
		if path != "" {
			return nil, fmt.Errorf("--labels and --no-labels together")
		}
		return nil, nil
	}
	if path == "" {
		path = labels.DefaultPath(reportPath)
		if _, err := os.Stat(path); err != nil {
			return nil, nil
		}
	} else if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	m, err := labels.Read(path)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(w, "using your labels from %s (%d frames; --no-labels to ignore)\n", path, len(m))
	return m, nil
}
