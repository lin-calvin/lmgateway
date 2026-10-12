package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"

	"lmgateway/internal/lmgcli"
)

// executeKeyCreate generates a data-plane API key server-side. The plaintext is
// returned exactly once by the gateway and never stored.
func executeKeyCreate(ctx context.Context, client *lmgcli.Client, args []string) ([]byte, error) {
	fs := flag.NewFlagSet("key", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var groups stringFlags
	fs.Var(&groups, "group", "group membership (repeatable)")
	args = normalizeResourceArgs(args)
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() != 1 {
		return nil, fmt.Errorf("usage: lmgcli key create ID [--group GROUP ...]")
	}
	body, err := json.Marshal(map[string]any{"id": fs.Arg(0), "groups": []string(groups)})
	if err != nil {
		return nil, err
	}
	return client.Action(ctx, "key/create", body)
}
