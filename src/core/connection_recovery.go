package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"syscall"
	"time"
)

const connectionRestoredMessage = "Connection to the ServiceNow instance was reauthenticated and restored. The interrupted turn was not retried automatically; send it again."

func turnConnectionNeedsRecovery(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, syscall.EPIPE) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	message := strings.ToLower(err.Error())
	for _, marker := range []string{
		"broken pipe", "connection closed", "connection reset", "use of closed network connection",
		"websocket: close", "websocket dial failed", "unexpected eof", "transport is closing",
		"user is not authenticated", "not authenticated", "unauthorized", "forbidden",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func connectionErrorRequiresCredentials(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errInvalidWebSession) {
		return true
	}
	message := strings.ToLower(err.Error())
	for _, marker := range []string{
		"(401)", "(403)", "status 401", "status 403", "unauthorized", "forbidden",
		"not authenticated", "authentication failed", "failed or expired",
		"no matching saved web session", "no valid cached oauth token", "saved credentials",
		"credential recovery", "requires credentials",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

// resetConnectionTransport closes one failed socket generation and waits for
// its reader before replacing channels. This prevents a late old reader from
// closing or signaling the newly established connection.
func (c *Client) resetConnectionTransport() {
	if c == nil {
		return
	}
	if c.nirvanaPingCancel != nil {
		c.nirvanaPingCancel()
		c.nirvanaPingCancel = nil
	}
	if c.ambCancel != nil {
		c.ambCancel()
		c.ambCancel = nil
	}
	c.ambSubscribed = false
	c.ambUnavailable = false
	c.ambClient = ""
	conn, closed := c.conn, c.closed
	if conn != nil {
		_ = conn.Close()
	}
	c.transportWG.Wait()
	if closed != nil {
		select {
		case <-closed:
		default:
			close(closed)
		}
	}
	c.conn = nil
	c.connected = make(chan error, 1)
	c.closed = make(chan struct{})
	c.turnDone = nil
	c.processing = false
	c.pendingUserContent = ""
	c.pendingUserAttachments = nil
	c.turnRuntimeSnapshot = nil
	c.resetWebStream()
}

func (c *Client) connectForRecovery(ctx context.Context) error {
	connectCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	return c.Connect(connectCtx)
}

// recoverConnectionAfterTurnError first attempts a noninteractive reconnect
// with existing session/OAuth state. Credentials are requested only when that
// attempt proves authentication is unusable. The interrupted user turn is not
// replayed because the remote side may have accepted it before the pipe broke.
func (c *Client) recoverConnectionAfterTurnError(ctx context.Context, cause error) (bool, error) {
	if c == nil || !turnConnectionNeedsRecovery(cause) {
		return false, nil
	}
	c.closeSemanticTurnForTransportFailure("connection_recovery")
	c.resetConnectionTransport()
	c.connectionAuthPrepared = false

	silentAuthErr := c.PrepareConnectionAuthenticationNonInteractive(ctx)
	var reconnectErr error
	if silentAuthErr == nil {
		reconnectErr = c.connectForRecovery(ctx)
		if reconnectErr == nil {
			return true, nil
		}
	}
	if !connectionErrorRequiresCredentials(silentAuthErr) && !connectionErrorRequiresCredentials(reconnectErr) {
		if reconnectErr != nil {
			return false, fmt.Errorf("connection recovery failed without an authentication challenge: %w", reconnectErr)
		}
		return false, fmt.Errorf("connection recovery could not validate existing authentication: %w", silentAuthErr)
	}

	c.resetConnectionTransport()
	deleteCachedToken(c.opts.Profile)
	if err := deleteWebSession(c.opts.Profile); err != nil {
		return false, err
	}
	c.clearWebSession()
	c.oauthAccessToken = ""
	c.basicPass = ""
	c.forceCredentialPrompt = c.gatewayAuth == authModeBasic
	defer func() { c.forceCredentialPrompt = false }()
	c.connectionAuthPrepared = false
	if err := c.PrepareConnectionAuthentication(ctx); err != nil {
		return false, fmt.Errorf("reauthentication failed: %w", err)
	}
	if err := c.connectForRecovery(ctx); err != nil {
		return false, fmt.Errorf("reconnection after reauthentication failed: %w", err)
	}
	return true, nil
}
