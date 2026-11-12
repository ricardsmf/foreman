package main

// foreman open: the Rex session foreman lives in, the board on the left and the queue on the
// right. The layout and its pane labels are foreman's, so a key binding in Rex only runs this.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
)

const (
	// homeSession is the label of foreman's own Rex session.
	homeSession = "foreman"
	// boardPane and queuePane label its two panes: the queue focuses the board by its label.
	boardPane = "board"
	queuePane = "queue"
	// homeRatio is the board's share of the width.
	homeRatio = 0.6
)

const openUsage = `usage: foreman open [--print]

Go to the foreman Rex session, the board on the left and the queue on the right: start it when it
isn't running, and give a running one the pane it lacks. --print prints the session's ID instead
of showing it in the Rex app, for a key binding that selects it in the app the key was pressed in.`

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

// openHome returns the foreman session's ID, starting the session when none is labelled so. A
// running one that has the board without the queue, or the queue without the board, gets the
// other beside it; one with neither is somebody's own and stays as it is.
func openHome(exe string) (string, error) {
	sessions, err := openSessions()
	if err != nil {
		return "", err
	}
	board := paneLayout(boardPane, home, []string{exe})
	queue := paneLayout(queuePane, home, []string{exe, "queue"})
	n := slices.IndexFunc(sessions, func(s rexSession) bool { return s.Label == homeSession })
	if n < 0 {
		return rex.createSession(homeSession, boardPane, sideBySide(homeRatio, board, queue))
	}
	s := sessions[n]
	pane := func(label string) string {
		for _, p := range s.Panes {
			if p.Placed && p.Label == label {
				return p.ID
			}
		}
		return ""
	}
	switch b, q := pane(boardPane), pane(queuePane); {
	case b != "" && q == "":
		err = rex.split(s.ID, b, "after", homeRatio, queue)
	case b == "" && q != "":
		err = rex.split(s.ID, q, "before", homeRatio, board)
	}
	return s.ID, err
}
