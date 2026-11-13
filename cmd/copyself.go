package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

// copySelfCommand is the name of the subcommand that copies the binary.
const copySelfCommand = "copy-self"

func newCopySelfCommand(b *base) *cobra.Command {
	return &cobra.Command{
		Use:    copySelfCommand + " <dest>",
		Short:  "Copy this binary to a file",
		Long:   "Copies the running binary to <dest> with mode 0755. The Helm chart runs it in an init container, because the image has no shell or cp, so that acp-host can run from a shared volume.",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			exe, err := b.executable()
			if err != nil {
				return err
			}
			if err := copyFile(exe, args[0]); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Copied %s to %s\n", exe, args[0])
			return err
		},
	}
}

// copyFile writes src to dst through a temporary file in the same directory,
// so a reader never sees a partial binary.
func copyFile(src, dst string) error {
	in, err := os.Open(src) //nolint:gosec // the running binary
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".talaria-copy-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o755); err != nil { //nolint:gosec // an executable
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}
