package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"gpb/internal/config"
	"gpb/internal/daemon"
	"gpb/internal/version"
)

const usage = `gpb — Google Photos backup

usage:
  gpb daemon                 run the web UI and the session keepalive loop
  gpb status [--healthcheck] report the Google session state of a running daemon
  gpb passwd                 set the web UI password
  gpb version                print the release this binary was built from

  gpb albums [--local]       list the albums, refreshing from Google unless --local
  gpb follow <id> [--mode all|picked]
                             back up an album; ids may be shortened to any unique prefix
  gpb unfollow <id>          stop backing up an album
  gpb sync [--limit N]       run one backup pass now
  gpb links                  rebuild <photos>/albums/, the symlink view of the pool
  gpb verify [--repair]      re-read the backed-up files and check them against their hashes
  gpb duplicates [--delete] [--link]
                             list photos kept as more than one file; --delete removes written-off
                             copies, --link makes the rest one hardlinked file each
  gpb probe [--again-after D] <key|file>...
                             ask Google for items as a run would and say what came back; changes nothing
`

func main() {
	log.SetFlags(log.LstdFlags)

	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	command, args := os.Args[1], os.Args[2:]
	if err := run(ctx, command, args); err != nil {
		log.Fatalf("gpb %s: %v", command, err)
	}
}

func run(ctx context.Context, command string, args []string) error {
	switch command {
	case "daemon":
		return runDaemon(ctx)
	case "status":
		return runStatus(args)
	case "passwd":
		return runPasswd()
	case "albums":
		return runAlbums(ctx, args)
	case "follow":
		return runFollow(args)
	case "unfollow":
		return runUnfollow(args)
	case "sync":
		return runSync(ctx, args)
	case "links":
		return runLinks(args)
	case "verify":
		return runVerify(ctx, args)
	case "duplicates":
		return runDuplicates(ctx, args)
	case "probe":
		return runProbe(ctx, args)
	case "version", "--version":
		fmt.Printf("gpb %s\n", version.Current)
		return nil
	case "-h", "--help", "help":
		fmt.Print(usage)
		return nil
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
		return nil
	}
}

func runDaemon(ctx context.Context) error {
	cfg, err := config.Load(config.DataDir())
	if err != nil {
		return err
	}

	service, err := daemon.New(cfg)
	if err != nil {
		return err
	}
	return service.Run(ctx)
}

func runStatus(args []string) error {
	flags := flag.NewFlagSet("status", flag.ExitOnError)
	healthcheck := flags.Bool("healthcheck", false, "exit non-zero unless the Google session is healthy")
	if err := flags.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(config.DataDir())
	if err != nil {
		return err
	}

	state, err := fetchState(cfg)
	if err != nil {
		return err
	}

	fmt.Println(state)
	if *healthcheck && state != healthyState {
		os.Exit(1)
	}
	return nil
}
