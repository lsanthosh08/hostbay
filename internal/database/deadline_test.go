package database

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

// noDeadlines is a connection that, like an SSH channel, has no deadlines.
type noDeadlines struct{ net.Conn }

func (noDeadlines) SetDeadline(time.Time) error      { return errors.New("deadline not supported") }
func (noDeadlines) SetReadDeadline(time.Time) error  { return errors.New("deadline not supported") }
func (noDeadlines) SetWriteDeadline(time.Time) error { return errors.New("deadline not supported") }

// A connection without deadlines takes them: a passed one ends a blocked
// read with a timeout, and a cleared one leaves the connection open.
func TestDeadlinesOnAConnectionWithoutThem(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	dial := withDeadlines(func(context.Context, string, string) (net.Conn, error) { return noDeadlines{client}, nil })
	c, err := dial(context.Background(), "tcp", "x:1")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SetReadDeadline(time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Time{})
	go server.Write([]byte("a"))
	if n, err := c.Read(make([]byte, 1)); n != 1 || err != nil {
		t.Fatalf("read after a cleared deadline: %d, %v", n, err)
	}
	c.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	start := time.Now()
	_, err = c.Read(make([]byte, 1))
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() || time.Since(start) > 2*time.Second {
		t.Fatalf("err = %v after %v, want a timeout", err, time.Since(start))
	}
}
