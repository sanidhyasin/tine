// Command tine-agent runs inside every microVM. It serves tined's requests,
// such as running commands, over vsock (docs/PLAN.md, step 1).
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/sanidhyasin/tine/internal/agent"
	"github.com/sanidhyasin/tine/internal/agentproto"
	"github.com/sanidhyasin/tine/internal/version"
)

func main() {
	listen := flag.String("listen", fmt.Sprintf("vsock:%d", agentproto.Port),
		`where to listen: "vsock:<port>", "unix:<path>" or "tcp:<host:port>"`)
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("tine-agent", version.String())
		return
	}
	if err := run(*listen); err != nil {
		fmt.Fprintln(os.Stderr, "tine-agent:", err)
		os.Exit(1)
	}
}

func run(listen string) error {
	// In the guest, stderr goes to the journal.
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	ln, err := agent.Listen(ctx, listen)
	if err != nil {
		return err
	}
	log.Info("tine-agent listening", "listen", listen, "version", version.Version, "protocol", agentproto.Version)

	srv := agent.New(agent.Config{Version: version.Version, Logger: log})
	if err := srv.Serve(ctx, ln); err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	log.Info("tine-agent stopped")
	return nil
}
