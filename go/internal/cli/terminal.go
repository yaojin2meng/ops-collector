package cli

import (
	"os"

	"golang.org/x/term"
)

func termIsTerminal() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

func termReadPassword() ([]byte, error) {
	return term.ReadPassword(int(os.Stdin.Fd()))
}
