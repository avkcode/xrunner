package client

import (
	"fmt"
	"os"
	"time"

	cliconfig "github.com/antonkrylov/xrunner/internal/cli/config"
)

type Connection struct {
	APIAddr     string
	Timeout     time.Duration
	ConfigPath  string
	ContextName string
	Config      *cliconfig.Config
	Context     *cliconfig.Context
}

// ResolveConnection mirrors cmd/xrunner's config semantics:
// 1) flags (apiAddr, timeout, contextName)
// 2) config file values
// 3) environment (XRUNNER_API_ADDR)
// 4) defaults (localhost:50051, 15s)
func ResolveConnection(configPath, contextName, apiAddr string, timeout time.Duration) (*Connection, error) {
	conn := &Connection{
		ConfigPath:  configPath,
		ContextName: contextName,
		APIAddr:     apiAddr,
		Timeout:     timeout,
	}

	if conn.ConfigPath != "" {
		cfg, err := cliconfig.Load(conn.ConfigPath)
		if err != nil {
			return nil, err
		}
		conn.Config = cfg
	}

	if conn.Config != nil {
		ctx, _, err := conn.Config.Resolve(conn.ContextName)
		if err != nil {
			return nil, err
		}
		conn.Context = ctx
	}

	if conn.APIAddr == "" && conn.Context != nil {
		conn.APIAddr = conn.Context.Server
	}

	if conn.Timeout == 0 {
		if conn.Context != nil && conn.Context.TimeoutSeconds > 0 {
			conn.Timeout = time.Duration(conn.Context.TimeoutSeconds) * time.Second
		} else {
			conn.Timeout = 15 * time.Second
		}
	}

	if conn.APIAddr == "" {
		conn.APIAddr = os.Getenv("XRUNNER_API_ADDR")
		if conn.APIAddr == "" {
			conn.APIAddr = "localhost:50051"
		}
	}

	if conn.APIAddr == "" {
		return nil, fmt.Errorf("api address is required")
	}

	return conn, nil
}
