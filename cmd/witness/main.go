package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/lenoir-dev/LenoirDesk/internal/recoverywitness"
	"github.com/lenoir-dev/LenoirDesk/internal/store"
)

func main() {
	if len(os.Args) != 2 || (os.Args[1] != "enroll" && os.Args[1] != "rebind" && os.Args[1] != "check") {
		fmt.Fprintln(os.Stderr, "usage: witness enroll|rebind|check (database URL via environment)")
		os.Exit(2)
	}
	url := os.Getenv("RELAYDESK_MIGRATION_DATABASE_URL")
	if os.Args[1] == "check" {
		url = os.Getenv("RELAYDESK_DATABASE_URL")
	}
	path := os.Getenv("RELAYDESK_RESTORE_WITNESS_PATH")
	reason := os.Getenv("RELAYDESK_RESTORE_WITNESS_REASON")
	if url == "" || path == "" || (os.Args[1] != "check" && reason == "") {
		fmt.Fprintln(os.Stderr, "witness maintenance configuration missing")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := store.Open(ctx, url)
	if err == nil {
		defer db.Close()
		witness := &recoverywitness.Witness{Pool: db.Pool, Path: path}
		if os.Args[1] == "check" {
			err = witness.Check(ctx)
		} else {
			err = witness.Bind(ctx, reason, os.Args[1] == "rebind")
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "external restore witness remains closed")
		os.Exit(1)
	}
	if os.Args[1] == "check" {
		fmt.Println("external restore witness verified")
	} else {
		fmt.Println("external restore witness bound; verify recovery gate before starting worker")
	}
}
