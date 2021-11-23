// Copyright 2020 The Fuchsia Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package sshutil

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"go.fuchsia.dev/fuchsia/tools/lib/retry"
	"golang.org/x/crypto/ssh"
)

func setUpClient(
	ctx context.Context,
	t *testing.T,
	onNewChannel func(ssh.NewChannel),
	onRequest func(*ssh.Request),
) (*Client, *sshServer) {
	server, err := startSSHServer(onNewChannel, onRequest)
	if err != nil {
		t.Fatalf("failed to start ssh server: %v", err)
	}
	t.Cleanup(server.stop)

	client, err := NewClient(
		ctx,
		ConstantAddrResolver{
			Addr: server.addr,
		},
		server.clientConfig,
		retry.NoRetries())
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	t.Cleanup(client.Close)

	return client, server
}

func TestReconnect(t *testing.T) {
	ctx := context.Background()

	t.Run("can run a command before and after reconnection", func(t *testing.T) {
		var execCount int64
		client, _ := setUpClient(
			ctx,
			t,
			onNewExecChannel(func(cmd string, stdout io.Writer, stderr io.Writer) int {
				expected := strconv.Itoa(int(atomic.AddInt64(&execCount, 1)))
				if expected != cmd {
					t.Fatalf("expected exec cmd to be %q, not %q", expected, cmd)
				}
				stdout.Write([]byte(expected))
				stderr.Write([]byte(expected))
				return 0
			}),
			nil,
		)

		// Check we can run a command before reconnecting.
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		if err := client.Run(ctx, []string{"1"}, &stdout, &stderr); err != nil {
			t.Errorf("failed to run a command: %v", err)
		}
		if execCount != 1 {
			t.Errorf("expected exec count to be 1, not %d", execCount)
		}
		if stdout.String() != "1" {
			t.Errorf("expected stdout to be \"1\", not %q", stdout.String())
		}
		if stderr.String() != "1" {
			t.Errorf("expected stderr to be \"1\", not %q", stdout.String())
		}

		disconnects := make(chan struct{})
		client.RegisterDisconnectListener(disconnects)

		client.Close()

		assertChannelClosed(t, disconnects, "close should have disconnected the client")

		if err := client.Reconnect(ctx); err != nil {
			t.Errorf("failed to reconnect: %v", err)
		}

		// Check we can still run a command after reconnecting.
		stdout.Reset()
		stderr.Reset()
		if err := client.Run(ctx, []string{"2"}, &stdout, &stderr); err != nil {
			t.Errorf("failed to run a command: %v", err)
		}
		if execCount != 2 {
			t.Errorf("expected exec count to be 2, not %d", execCount)
		}
		if stdout.String() != "2" {
			t.Errorf("expected stdout to be \"2\", not %q", stdout.String())
		}
		if stderr.String() != "2" {
			t.Errorf("expected stderr to be \"2\", not %q", stdout.String())
		}
	})
}

func TestCloseDuringConnection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serverConfig, clientConfig, err := genSSHConfig()
	if err != nil {
		t.Fatalf("failed to create ssh config: %v", err)
	}

	connected := make(chan struct{})
	reconnected := make(chan struct{})
	done := make(chan struct{})
	defer close(done)

	// Spawn a server goroutine that will accept a connection, but never read
	// or write to the socket.
	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("failed to listen on port: %v", err)
	}
	defer listener.Close()

	serverErrs := make(chan error)
	go func() {
		// `NewClient` blocks until the first connection is made, so
		// we'll accept then close the connection.
		tcpConn, err := listener.Accept()
		if err != nil {
			serverErrs <- err
			return
		}

		sshConn, _, _, err := ssh.NewServerConn(tcpConn, serverConfig)
		if err != nil {
			serverErrs <- err
			return
		}

		<-connected

		sshConn.Close()

		// Accept the reconnection attempt, but never respond to it.
		tcpConn, err = listener.Accept()
		if err != nil {
			serverErrs <- err
			return
		}

		close(reconnected)

		// Wait for the test to complete before closing the
		// connection. Otherwise we'll race with the OS
		// observing the closed connection with the context to
		// be canceled.
		<-done

		tcpConn.Close()
	}()

	// Spawn a goroutine to establish the initial client.
	type clientResult struct {
		client *Client
		err    error
	}
	clientChan := make(chan clientResult)

	go func() {
		client, err := NewClient(
			ctx,
			ConstantAddrResolver{
				Addr: listener.Addr(),
			},
			clientConfig,
			retry.NoRetries(),
		)
		clientChan <- clientResult{
			client: client,
			err:    err,
		}
	}()

	// Wait for the connection to succeed.
	var client *Client
	select {
	case <-time.After(testTimeout):
		t.Fatalf("server didn't accept the connection in time")
	case err := <-serverErrs:
		t.Fatalf("server failed to accept connection: %v", err)
	case res := <-clientChan:
		if res.err != nil {
			t.Fatalf("client failed to connect: %v", err)
		}

		client = res.client
	}

	// Signal to the server that we connected so it can switch to waiting
	// for the reconnection attempt.
	close(connected)

	reconnectCtx, reconnectCancel := context.WithCancel(ctx)
	reconnectErrs := make(chan error)

	// Spawn a reconnection attempt. This should never succeed
	go func() {
		reconnectErrs <- client.Reconnect(reconnectCtx)
	}()

	select {
	case <-time.After(testTimeout):
		t.Fatalf("client never attempted to reconnect")
	case err := <-serverErrs:
		t.Fatalf("server failed to accept reconnection: %v", err)
	case err := <-reconnectErrs:
		t.Fatalf("client failed to reconnect: %v", err)
	case <-reconnected:
	}

	// Close the connection, which should not block.
	clientClosed := make(chan struct{})
	go func() {
		client.Close()
		close(clientClosed)
	}()

	// The close goroutine should succeed.
	select {
	case <-time.After(testTimeout):
		t.Fatalf("client failed to close")
	case <-clientClosed:
	}

	// The reconnection goroutine won't error out until the context is canceled.
	reconnectCancel()

	select {
	case <-time.After(testTimeout):
		t.Fatalf("client reconnection never failed")
	case err := <-reconnectErrs:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("client reconnection should have been canceled, not %v", err)
		}
	}
}
