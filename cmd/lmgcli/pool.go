package main

import (
	"context"
	"fmt"

	"lmgateway/internal/lmgcli"
)

// executePool handles the pool command group. `status`, `rotate` and `clear` are
// runtime operations; everything else is the generic config entity CRUD.
func executePool(ctx context.Context, client *lmgcli.Client, operation string, args []string) ([]byte, error) {
	switch operation {
	case "status":
		name, err := poolName(args, "status")
		if err != nil {
			return nil, err
		}
		return client.PoolStatus(ctx, name)
	case "rotate":
		name, err := poolName(args, "rotate")
		if err != nil {
			return nil, err
		}
		return client.PoolRotate(ctx, name)
	case "clear":
		name, err := poolName(args, "clear")
		if err != nil {
			return nil, err
		}
		return client.PoolClear(ctx, name)
	}
	name, body, ifMatch, err := parseResourceArgs(args, operation)
	if err != nil {
		return nil, err
	}
	return client.Resource(ctx, "pool", operation, name, body, ifMatch)
}

func poolName(args []string, operation string) (string, error) {
	name, _, _, err := parseResourceArgs(args, "get")
	if err != nil {
		return "", err
	}
	if name == "" {
		return "", fmt.Errorf("usage: lmgcli pool %s MODEL", operation)
	}
	return name, nil
}
