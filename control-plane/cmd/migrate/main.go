// Command migrate applies or reverts the embedded PostgreSQL migrations.
//
//	migrate up            apply all pending migrations
//	migrate down [N]      revert the last N migrations (default 1)
//	migrate status        print applied and latest versions
package main

import (
	"context"
	"fmt"
	"os"
	"strconv"

	"github.com/systera/control-plane/internal/platform/config"
	"github.com/systera/control-plane/internal/platform/postgres"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: migrate up | down [N] | status")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx := context.Background()
	pool, err := postgres.Open(ctx, cfg.PostgresDSN())
	if err != nil {
		return err
	}
	defer pool.Close()

	switch args[0] {
	case "up":
		applied, err := postgres.MigrateUp(ctx, pool)
		if err != nil {
			return err
		}
		if len(applied) == 0 {
			fmt.Println("schema up to date")
		}
		for _, v := range applied {
			fmt.Printf("applied %06d\n", v)
		}
	case "down":
		steps := 1
		if len(args) > 1 {
			if steps, err = strconv.Atoi(args[1]); err != nil || steps < 1 {
				return fmt.Errorf("down: N must be a positive integer")
			}
		}
		reverted, err := postgres.MigrateDown(ctx, pool, steps)
		if err != nil {
			return err
		}
		for _, v := range reverted {
			fmt.Printf("reverted %06d\n", v)
		}
	case "status":
		have, err := postgres.CurrentVersion(ctx, pool)
		if err != nil {
			return err
		}
		want, err := postgres.LatestVersion()
		if err != nil {
			return err
		}
		fmt.Printf("applied=%06d latest=%06d\n", have, want)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
	return nil
}
