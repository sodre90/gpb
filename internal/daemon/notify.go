package daemon

import (
	"context"
	"log"
	"os/exec"
	"time"
)

const notifyTimeout = 30 * time.Second

// notifyPipeGrace bounds the other way a hook can hang, which is not the one the timeout
// covers. CombinedOutput waits for the output pipes to close rather than for the process to
// exit, so an ordinary `something &` in a hook script leaves a grandchild holding stdout and
// the wait lasts as long as that grandchild does. The timeout is no help there: the hook
// itself has already exited, so cancelling has nothing left to kill.
//
// This is charged to the run goroutine, which is what makes it worth guarding rather than
// merely untidy — the backup never finishes, the one-run-at-a-time rule then refuses every
// later run, and the daemon goes on looking perfectly healthy.
const notifyPipeGrace = time.Second

// Notify runs the user's hook as `<command> <event> <message>`. A hook that fails or hangs
// is logged and dropped: notification is never allowed to hold up the daemon.
func (d *Daemon) Notify(event, message string) {
	log.Printf("event: %s — %s", event, message)

	command := d.cfg.Notify.Command
	if command == "" {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), notifyTimeout)
	defer cancel()

	hook := exec.CommandContext(ctx, command, event, message)
	hook.WaitDelay = notifyPipeGrace

	output, err := hook.CombinedOutput()
	if err != nil {
		log.Printf("daemon: notify hook failed: %v: %s", err, output)
	}
}
