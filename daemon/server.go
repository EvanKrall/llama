package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/rpc"
	"os"
	"path"
	"syscall"
)

func SocketPath() string {
	if sock := os.Getenv("LLAMA_SOCKET"); sock != "" {
		return sock
	}
	if home := os.Getenv("HOME"); home != "" {
		return path.Join(home, ".llama", "llama.sock")
	}
	return fmt.Sprintf("/run/llama-%d/llama.sock", os.Getuid())
}

var ErrAlreadyRunning = errors.New("daemon already running")

func Start(ctx context.Context) error {
	sockPath := SocketPath()
	if err := os.MkdirAll(path.Dir(sockPath), 0700); err != nil {
		return err
	}
	listener, err := net.Listen("unix", sockPath)
	if err != nil && errors.Is(err, syscall.EADDRINUSE) {
		var client *Client
		// The socket exists. Is someone listening?
		client, err = Dial(ctx)
		if err == nil {
			_, err = client.Ping(&PingInput{})
			if err == nil {
				return ErrAlreadyRunning
			}
			return err
		}
		// TODO: be atomic (lockfile?) if multiple clients hit
		// this path at once.
		if err := os.Remove(sockPath); err != nil {
			return err
		}
		listener, err = net.Listen("unix", sockPath)
	}
	if err != nil {
		return err
	}
	var httpSrv http.Server
	var rpcSrv rpc.Server
	rpcSrv.Register(&Daemon{})
	httpSrv.Handler = &rpcSrv
	go func() {
		httpSrv.Serve(listener)
	}()
	<-ctx.Done()
	httpSrv.Shutdown(ctx)
	return nil
}

func Dial(_ context.Context) (*Client, error) {
	conn, err := rpc.DialHTTP("unix", SocketPath())
	if err != nil {
		return nil, err
	}
	return &Client{conn}, nil
}
