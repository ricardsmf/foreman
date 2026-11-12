package main

// foreman open: the Rex session foreman lives in, one pane running the board and the queue. A key
// binding in Rex only runs this.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
)

// homeSession is the label of foreman's own Rex session, and of its pane.
const homeSession = "foreman"

const openUsage = `usage: foreman open [--print]

Go to the foreman Rex session, starting it when it isn't running: one pane with the board and the
queue. --print prints the session's ID instead of showing it in the Rex app, for a key binding
that selects it in the app the key was pressed in.`

func runOpen(argv []string, out io.Writer) error {
	if len(argv) == 1 && (argv[0] == "--help" || argv[0] == "-h") {
		fmt.Fprintln(out, openUsage)
		return nil
	}
	print := len(argv) == 1 && argv[0] == "--print"
	if len(argv) > 1 || (len(argv) == 1 && !print) {
		return errors.New(openUsage)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	id, err := openHome(exe)
	if err != nil {
		return err
	}
	if print {
		fmt.Fprintln(out, id)
		return nil
	}
	return rex.show(id)
}

// openHome returns the foreman session's ID, starting the session when none is labelled so.
func openHome(exe string) (string, error) {
	sessions, err := openSessions()
	if err != nil {
		return "", err
	}
	if n := slices.IndexFunc(sessions, func(s rexSession) bool { return s.Label == homeSession }); n >= 0 {
		return sessions[n].ID, nil
	}
	return rex.createSession(homeSession, homeSession, paneLayout(homeSession, home, []string{exe}))
}
