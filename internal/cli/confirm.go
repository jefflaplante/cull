package cli

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// confirmAbove is the estimate above which judge and rank ask before spending.
const confirmAbove = 1.0

// isTerminal reports whether f is an interactive terminal (a test hook).
var isTerminal = func(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// confirmSpend asks on a terminal before a run estimated above confirmAbove: a
// mistyped re-run shouldn't spend real money unasked. Without a terminal (a script,
// an agent) there is nobody to ask, and it proceeds as before; yes (--yes) skips it.
func confirmSpend(cmd *cobra.Command, usd float64, yes bool) error {
	if yes || usd <= confirmAbove || !isTerminal(os.Stdin) {
		return nil
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "Spend about $%.2f? [y/N] ", usd)
	line, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return nil
	}
	return fmt.Errorf("not confirmed: nothing was spent (pass --yes to skip this question)")
}
